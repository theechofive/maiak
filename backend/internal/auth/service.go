// Package auth owns users, memberships, auth identities and sessions: the admin bootstrap from
// the Secret, self-service registration, password login, server-side sessions with CSRF tokens,
// and the login rate limit.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/audit"
	"github.com/theechofive/maiak/backend/internal/auth/authdb"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/tenants"
)

// Roles of a membership.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleViewer = "viewer"
)

// Audit actions written by this module.
const (
	ActionLoginSucceeded = "auth.login.succeeded"
	ActionLoginFailed    = "auth.login.failed"
	ActionLogout         = "auth.logout"
	ActionAdminRotated   = "admin.rotated"
	ActionRegister       = "auth.register"
)

const (
	userStatusActive = "active"
	// touchInterval throttles last_seen_at updates to one write per session per minute.
	touchInterval = time.Minute
	// maxConcurrentHashes bounds argon2 memory: each verification allocates the hash's m
	// (64 MiB by default) and the api pod limit is a few hundred MiB.
	maxConcurrentHashes = 2
	// defaultHashQueueWait bounds how long a login waits for a verification slot; beyond it the
	// request is rejected as rate limited instead of piling up goroutines and connections.
	defaultHashQueueWait = 5 * time.Second
	maxUserAgent         = 512
	// unattributedIPKey is the limiter key for requests without a usable client address (for
	// example a missing X-Forwarded-For behind a trusted proxy). They share one bucket instead
	// of the proxy's address or an "invalid IP" string.
	unattributedIPKey = "ip:unattributed"
	// bootstrapLockKey serialises BootstrapAdmin across replicas (transaction advisory lock).
	bootstrapLockKey int64 = 0x6d61_6961_6b61_646d // "maiakadm"
)

// Options configure a Service.
type Options struct {
	SessionTTL time.Duration
	Clock      clock.Clock
	Logger     *slog.Logger
	Registerer prometheus.Registerer
	// IPLimiter and LoginLimiter default to 30 and 10 failures per 15 minutes.
	IPLimiter    *Limiter
	LoginLimiter *Limiter
	// HashQueueWait defaults to 5 s.
	HashQueueWait time.Duration
}

// Service implements authentication.
type Service struct {
	pool       *db.Pool
	ttl        time.Duration
	clock      clock.Clock
	log        *slog.Logger
	ipLimit    *Limiter
	loginLimit *Limiter
	hashSem    chan struct{}
	hashWait   time.Duration
	// dummy is verified for unknown or password-less users. BootstrapAdmin builds it with the
	// admin hash's parameters, so both cost the same and timing does not reveal the login.
	dummy         *crypto.ParsedHash
	logins        *prometheus.CounterVec
	registrations *prometheus.CounterVec

	// defaultTenant is set by BootstrapAdmin. Failed logins of unknown users are audited in it.
	defaultTenant uuid.UUID
}

// NewService builds the service. Call BootstrapAdmin before serving logins.
func NewService(pool *db.Pool, opts Options) (*Service, error) {
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.IPLimiter == nil {
		opts.IPLimiter = NewLimiter(30, 15*time.Minute, 100_000, opts.Clock)
	}
	if opts.LoginLimiter == nil {
		opts.LoginLimiter = NewLimiter(10, 15*time.Minute, 100_000, opts.Clock)
	}
	if opts.HashQueueWait <= 0 {
		opts.HashQueueWait = defaultHashQueueWait
	}
	s := &Service{
		pool:       pool,
		ttl:        opts.SessionTTL,
		clock:      opts.Clock,
		log:        opts.Logger,
		ipLimit:    opts.IPLimiter,
		loginLimit: opts.LoginLimiter,
		hashSem:    make(chan struct{}, maxConcurrentHashes),
		hashWait:   opts.HashQueueWait,
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_auth_logins_total",
			Help: "Login attempts by result (succeeded, failed, suspended, rate_limited).",
		}, []string{"result"}),
		registrations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_auth_registrations_total",
			Help: "Registration attempts by result (succeeded, invalid, email_taken, rate_limited).",
		}, []string{"result"}),
	}
	if opts.Registerer != nil {
		for _, c := range []prometheus.Collector{s.logins, s.registrations} {
			if err := opts.Registerer.Register(c); err != nil {
				return nil, fmt.Errorf("auth: register metrics: %w", err)
			}
		}
	}
	return s, nil
}

// BootstrapAdmin upserts the default tenant (created on the internal plan; an existing tenant
// keeps its plan), the admin user and its owner membership in one
// transaction. It is idempotent and safe to run from several replicas at once. When the hash
// in the Secret changes, the stored hash is replaced (this is how the password is reset) and
// the admin's sessions are deleted. When the login changes, the previous owners of the default
// tenant are disabled and their sessions deleted. The login is stored lowercased in users.email
// (the column holds the login identifier, ADR-0009).
//
// An existing user with that login is adopted only when all its memberships are in the default
// tenant; otherwise BootstrapAdmin writes nothing and returns ErrAdminLoginInUse (a stranger may
// have registered the email before the operator configured it, ADR-0095). An adopted user loses
// every session it had, in every tenant.
func (s *Service) BootstrapAdmin(ctx context.Context, login, passwordHash string) error {
	login = normalizeLogin(login)
	parsed, err := crypto.ParseArgon2idHash(passwordHash)
	if err != nil {
		return fmt.Errorf("auth: admin password hash: %w", err)
	}
	dummyPHC, err := crypto.HashPassword(uuidv7.New().String(), parsed.Params)
	if err != nil {
		return fmt.Errorf("auth: dummy hash: %w", err)
	}
	dummy, err := crypto.ParseArgon2idHash(dummyPHC)
	if err != nil {
		return fmt.Errorf("auth: dummy hash: %w", err)
	}
	var tenantID uuid.UUID
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", bootstrapLockKey); err != nil {
			return fmt.Errorf("bootstrap lock: %w", err)
		}
		t, err := tenants.Ensure(ctx, tx, tenants.DefaultSlug, "Default")
		if err != nil {
			return err
		}
		tenantID = t.ID
		if err := db.SetTenant(ctx, tx, t.ID); err != nil {
			return err
		}
		q := authdb.New(tx)
		adopted, err := s.checkAdoption(ctx, tx, q, t.ID, login)
		if err != nil {
			return err
		}
		if err := q.InsertUserIfMissing(ctx, authdb.InsertUserIfMissingParams{
			ID: uuidv7.New(), Email: login, PasswordHash: &passwordHash, DisplayName: "Admin",
		}); err != nil {
			return fmt.Errorf("insert admin user: %w", err)
		}
		u, err := q.GetUserByEmail(ctx, login)
		if err != nil {
			return fmt.Errorf("get admin user: %w", err)
		}
		changed, err := q.UpdateUserPasswordHash(ctx, authdb.UpdateUserPasswordHashParams{ID: u.ID, PasswordHash: &passwordHash})
		if err != nil {
			return fmt.Errorf("update admin password hash: %w", err)
		}
		if changed > 0 || adopted {
			n, err := revokeUserSessions(ctx, tx, u.ID)
			if err != nil {
				return fmt.Errorf("revoke admin sessions: %w", err)
			}
			s.log.InfoContext(ctx, "admin password hash or user changed from configuration; sessions revoked",
				"user_id", u.ID, "adopted", adopted, "sessions", n)
		}
		// A configured admin is always active (rotating back to an earlier login re-enables it).
		if _, err := q.ActivateUser(ctx, u.ID); err != nil {
			return fmt.Errorf("activate admin user: %w", err)
		}
		if err := q.UpsertMembership(ctx, authdb.UpsertMembershipParams{TenantID: t.ID, UserID: u.ID, Role: RoleOwner}); err != nil {
			return fmt.Errorf("upsert admin membership: %w", err)
		}
		if err := s.retirePreviousOwners(ctx, tx, q, t.ID, u.ID); err != nil {
			return err
		}
		return s.syncPlatformAdmin(ctx, tx, u.ID)
	})
	if err != nil {
		return fmt.Errorf("auth: bootstrap admin: %w", err)
	}
	s.dummy = dummy
	s.defaultTenant = tenantID
	return nil
}

// checkAdoption looks up an existing user with the configured login. It fails with
// ErrAdminLoginInUse when that user belongs to any tenant other than the default one, and
// reports whether the user is being adopted (it exists but is not yet the active owner of the
// default tenant). The membership count goes through auth_count_other_memberships, because the
// memberships policy only shows the default tenant here.
func (s *Service) checkAdoption(ctx context.Context, tx pgx.Tx, q *authdb.Queries, tenantID uuid.UUID, login string) (bool, error) {
	u, err := q.GetUserByEmail(ctx, login)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get admin user: %w", err)
	}
	var others int
	if err := tx.QueryRow(ctx, `SELECT auth_count_other_memberships($1, $2)`, u.ID, tenantID).Scan(&others); err != nil {
		return false, fmt.Errorf("count admin memberships: %w", err)
	}
	if others > 0 {
		s.log.ErrorContext(ctx, "MAIAK_ADMIN_LOGIN names a user with memberships outside the default tenant; not adopting it",
			"user_id", u.ID, "other_memberships", others)
		return false, fmt.Errorf("%w (user %s)", ErrAdminLoginInUse, u.ID)
	}
	role, err := q.GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: tenantID, UserID: u.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get admin membership: %w", err)
	}
	return role != RoleOwner || u.Status != userStatusActive, nil
}

// revokeUserSessions deletes every session of the user in every tenant (auth_revoke_user_sessions).
func revokeUserSessions(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT auth_revoke_user_sessions($1)`, userID).Scan(&n)
	return n, err
}

// syncPlatformAdmin makes the configured admin the only platform admin (auth_sync_platform_admin,
// ADR-0097): its flag is set at every start and cleared for everyone else, a retired previous
// admin included. No API changes the flag.
func (s *Service) syncPlatformAdmin(ctx context.Context, tx pgx.Tx, adminID uuid.UUID) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT auth_sync_platform_admin($1)`, adminID).Scan(&n); err != nil {
		return fmt.Errorf("sync platform admin flag: %w", err)
	}
	if n > 0 {
		s.log.InfoContext(ctx, "platform admin flag synchronised with MAIAK_ADMIN_LOGIN", "user_id", adminID, "users_changed", n)
	}
	return nil
}

// retirePreviousOwners disables every other active owner of the tenant (the admin from an
// earlier MAIAK_ADMIN_LOGIN), deletes their sessions in every tenant and audits it.
func (s *Service) retirePreviousOwners(ctx context.Context, tx pgx.Tx, q *authdb.Queries, tenantID, adminID uuid.UUID) error {
	others, err := q.ListOtherActiveOwners(ctx, authdb.ListOtherActiveOwnersParams{TenantID: tenantID, UserID: adminID})
	if err != nil {
		return fmt.Errorf("list previous owners: %w", err)
	}
	for _, old := range others {
		if err := q.DisableUser(ctx, old); err != nil {
			return fmt.Errorf("disable previous owner: %w", err)
		}
		n, err := revokeUserSessions(ctx, tx, old)
		if err != nil {
			return fmt.Errorf("revoke previous owner sessions: %w", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, Action: ActionAdminRotated, EntityType: "user", EntityID: old.String(),
			Metadata: map[string]any{"new_admin_user_id": adminID.String(), "sessions_revoked": n},
		}); err != nil {
			return err
		}
		s.log.InfoContext(ctx, "previous admin disabled after MAIAK_ADMIN_LOGIN change", "user_id", old, "new_user_id", adminID)
	}
	return nil
}

// LoginInput is a login attempt.
type LoginInput struct {
	Login     string
	Password  string
	IP        netip.Addr
	UserAgent string
}

// User is the public view of a user.
type User struct {
	ID          uuid.UUID
	Login       string
	DisplayName string
	// IsPlatformAdmin is set only for the configured system admin (ADR-0097).
	IsPlatformAdmin bool
}

// SessionInfo describes the current session.
type SessionInfo struct {
	User      User
	Tenant    tenants.Tenant
	Role      string
	CSRFToken string
	ExpiresAt time.Time
}

// LoginResult is a successful login. Token goes into the cookie and is never stored.
type LoginResult struct {
	Token string
	SessionInfo
}

// Principal is an authenticated session.
type Principal struct {
	SessionID uuid.UUID
	UserID    uuid.UUID
	TenantID  uuid.UUID
	CSRFToken string
	ExpiresAt time.Time
}

// Login verifies credentials and creates a session.
func (s *Service) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	if s.defaultTenant == uuid.Nil {
		return LoginResult{}, errors.New("auth: BootstrapAdmin has not run")
	}
	ipKey := limiterIPKey(in.IP)
	loginKey := normalizeLogin(in.Login)
	// Attempts are reserved before the verification and stay counted when it fails; a success
	// or an attempt that never reached a verdict is refunded.
	if ok, wait := s.ipLimit.Reserve(ipKey); !ok {
		return LoginResult{}, s.loginRateLimited(ctx, wait)
	}
	if ok, wait := s.loginLimit.Reserve(loginKey); !ok {
		s.ipLimit.Refund(ipKey)
		return LoginResult{}, s.loginRateLimited(ctx, wait)
	}
	refund := func() {
		s.ipLimit.Refund(ipKey)
		s.loginLimit.Refund(loginKey)
	}

	user, tenantID, ok, err := s.checkPassword(ctx, loginKey, in.Password)
	if err != nil {
		refund()
		if errors.Is(err, ErrRateLimited) {
			s.logins.WithLabelValues("rate_limited").Inc()
		}
		return LoginResult{}, err
	}
	// Failures are audited in the user's tenant when there is one, otherwise in the default tenant.
	auditTenant := s.defaultTenant
	if tenantID != uuid.Nil {
		auditTenant = tenantID
	}
	if !ok {
		s.logins.WithLabelValues("failed").Inc()
		var actor *uuid.UUID
		if user != nil {
			actor = &user.ID
		}
		// The attempted login is not recorded: users sometimes type a password into it.
		if err := s.auditFailure(ctx, auditTenant, actor, in.IP, "invalid_credentials"); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}

	var res LoginResult
	err = errNoMembership
	if tenantID != uuid.Nil {
		err = db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
			q := authdb.New(tx)
			role, err := q.GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: tenantID, UserID: user.ID})
			if errors.Is(err, pgx.ErrNoRows) {
				return errNoMembership
			}
			if err != nil {
				return fmt.Errorf("get membership: %w", err)
			}
			t, err := tenants.Get(ctx, tx, tenantID)
			if errors.Is(err, tenants.ErrNotFound) {
				// auth_login_tenant returns a deleted tenant only when the user has no live one.
				return errSuspended
			}
			if err != nil {
				return err
			}
			if t.Status != tenants.StatusActive {
				return errSuspended
			}
			sess, err := s.insertSession(ctx, q, tenantID, user.ID, in.IP, in.UserAgent)
			if err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorUserID: &user.ID, Action: ActionLoginSucceeded,
				EntityType: "session", EntityID: sess.id.String(), IP: in.IP,
			}); err != nil {
				return err
			}
			res = LoginResult{Token: sess.token, SessionInfo: SessionInfo{
				User: *user, Tenant: t, Role: role, CSRFToken: sess.csrf, ExpiresAt: sess.expiresAt,
			}}
			return nil
		})
	}
	if errors.Is(err, errNoMembership) {
		s.logins.WithLabelValues("failed").Inc()
		if aerr := s.auditFailure(ctx, auditTenant, &user.ID, in.IP, "no_active_membership"); aerr != nil {
			return LoginResult{}, aerr
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	if errors.Is(err, errSuspended) {
		// Only reached with the right password, so the state of the tenant is never revealed to
		// a guess. The attempt stays counted by the limiters like a failure.
		s.logins.WithLabelValues("suspended").Inc()
		if aerr := s.auditFailure(ctx, auditTenant, &user.ID, in.IP, "tenant_suspended"); aerr != nil {
			return LoginResult{}, aerr
		}
		return LoginResult{}, ErrAccountSuspended
	}
	if err != nil {
		refund()
		return LoginResult{}, fmt.Errorf("auth: login: %w", err)
	}
	s.ipLimit.Refund(ipKey)
	s.loginLimit.Reset(loginKey)
	s.logins.WithLabelValues("succeeded").Inc()
	return res, nil
}

// errNoMembership means the credentials are right but the user has no membership. The caller
// answers ErrInvalidCredentials.
var errNoMembership = errors.New("auth: no active membership")

// errSuspended means the credentials are right but the user's tenant is suspended or deleted.
// The caller answers ErrAccountSuspended.
var errSuspended = errors.New("auth: tenant suspended")

// checkPassword looks the user and the tenant the login binds to up and verifies the password.
// It always runs one argon2 verification (against a dummy hash for unknown or password-less
// users), so the response time does not reveal whether the login exists. tenantID is uuid.Nil
// when the user is unknown or has no membership in a live tenant.
func (s *Service) checkPassword(ctx context.Context, login, password string) (*User, uuid.UUID, bool, error) {
	var row *authdb.GetUserByEmailRow
	var tenantID *uuid.UUID
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := authdb.New(tx).GetUserByEmail(ctx, login)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		row = &r
		// Hand-written like auth_lookup_session: a SECURITY DEFINER function, because the
		// memberships policy hides every row until the tenant is known.
		return tx.QueryRow(ctx, `SELECT auth_login_tenant($1)`, r.ID).Scan(&tenantID)
	})
	if err != nil {
		return nil, uuid.Nil, false, fmt.Errorf("auth: get user: %w", err)
	}

	hash := s.dummy
	if row != nil && row.PasswordHash != nil {
		if h, perr := crypto.ParseArgon2idHash(*row.PasswordHash); perr == nil {
			hash = h
		} else {
			s.log.ErrorContext(ctx, "stored password hash is invalid", "user_id", row.ID)
		}
	}
	if err := s.acquireHash(ctx); err != nil {
		return nil, uuid.Nil, false, err
	}
	match := hash.Verify(password)
	<-s.hashSem

	if row == nil {
		return nil, uuid.Nil, false, nil
	}
	u := &User{ID: row.ID, Login: row.Email, DisplayName: row.DisplayName, IsPlatformAdmin: row.IsPlatformAdmin}
	ok := match && hash != s.dummy && row.Status == userStatusActive
	if tenantID == nil {
		return u, uuid.Nil, ok, nil
	}
	return u, *tenantID, ok, nil
}

// normalizeLogin is the one place logins are made case-insensitive: bootstrap, lookup and
// rate-limit keys all use it, so "Admin" and "admin" are the same user and the same bucket.
func normalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// limiterIPKey is the per-client limiter key; requests without a usable address share one.
func limiterIPKey(ip netip.Addr) string {
	if ip.IsValid() {
		return "ip:" + ip.String()
	}
	return unattributedIPKey
}

// acquireHash takes a slot of the argon2 queue shared by login and registration, waiting at
// most hashWait; a full queue is a *RateLimitedError (the caller counts and logs it).
func (s *Service) acquireHash(ctx context.Context) error {
	t := time.NewTimer(s.hashWait)
	defer t.Stop()
	select {
	case s.hashSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		s.log.WarnContext(ctx, "password hash queue full; request rate limited")
		return &RateLimitedError{RetryAfter: s.hashWait}
	}
}

func (s *Service) loginRateLimited(ctx context.Context, wait time.Duration) error {
	s.logins.WithLabelValues("rate_limited").Inc()
	// Not audited: a flood of attempts would turn into a flood of audit rows.
	s.log.WarnContext(ctx, "login rate limited")
	return &RateLimitedError{RetryAfter: wait}
}

// session is a freshly created session; token goes into the cookie and is never stored.
type session struct {
	id        uuid.UUID
	token     string
	csrf      string
	expiresAt time.Time
}

// insertSession creates a session of the membership (tenantID, userID) in the caller's tenant
// transaction.
func (s *Service) insertSession(ctx context.Context, q *authdb.Queries, tenantID, userID uuid.UUID, ip netip.Addr, userAgent string) (session, error) {
	token, err := newToken()
	if err != nil {
		return session{}, err
	}
	csrf, err := newToken()
	if err != nil {
		return session{}, err
	}
	now := s.clock.Now()
	params := authdb.InsertSessionParams{
		ID: uuidv7.New(), UserID: userID, TenantID: tenantID, TokenHash: hashToken(token),
		CsrfToken: csrf, CreatedAt: now, ExpiresAt: now.Add(s.ttl),
	}
	if ip.IsValid() {
		params.Ip = &ip
	}
	if ua := truncate(userAgent, maxUserAgent); ua != "" {
		params.UserAgent = &ua
	}
	if err := q.InsertSession(ctx, params); err != nil {
		return session{}, fmt.Errorf("insert session: %w", err)
	}
	return session{id: params.ID, token: token, csrf: csrf, expiresAt: params.ExpiresAt}, nil
}

func (s *Service) auditFailure(ctx context.Context, tenantID uuid.UUID, actor *uuid.UUID, ip netip.Addr, reason string) error {
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorUserID: actor, Action: ActionLoginFailed,
			EntityType: "user", Metadata: map[string]any{"reason": reason}, IP: ip,
		})
	})
	if err != nil {
		return fmt.Errorf("auth: audit login failure: %w", err)
	}
	return nil
}

// Authenticate resolves a session token to a principal. It refreshes last_seen_at at most
// once per touchInterval.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if !validTokenFormat(token) {
		return Principal{}, ErrUnauthenticated
	}
	var p Principal
	var lastSeen time.Time
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Hand-written: sqlc cannot infer the columns of a RETURNS TABLE function.
		return tx.QueryRow(ctx,
			`SELECT id, user_id, tenant_id, csrf_token, expires_at, last_seen_at FROM auth_lookup_session($1)`,
			hashToken(token),
		).Scan(&p.SessionID, &p.UserID, &p.TenantID, &p.CSRFToken, &p.ExpiresAt, &lastSeen)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: lookup session: %w", err)
	}
	now := s.clock.Now()
	if now.Sub(lastSeen) >= touchInterval {
		err := db.WithTenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
			return authdb.New(tx).TouchSession(ctx, authdb.TouchSessionParams{ID: p.SessionID, Now: now, StaleBefore: now.Add(-touchInterval)})
		})
		if err != nil {
			// Not fatal: the session is valid, only the activity timestamp is stale.
			s.log.WarnContext(ctx, "touch session failed", "error", err)
		}
	}
	return p, nil
}

// Me returns the session info for p.
func (s *Service) Me(ctx context.Context, p Principal) (SessionInfo, error) {
	var info SessionInfo
	err := db.WithTenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		u, err := q.GetUserByID(ctx, p.UserID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if u.Status != userStatusActive {
			return ErrUnauthenticated
		}
		role, err := q.GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: p.TenantID, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		if err != nil {
			return fmt.Errorf("get membership: %w", err)
		}
		t, err := tenants.Get(ctx, tx, p.TenantID)
		if err != nil {
			return err
		}
		info = SessionInfo{
			User:   User{ID: u.ID, Login: u.Email, DisplayName: u.DisplayName, IsPlatformAdmin: u.IsPlatformAdmin},
			Tenant: t, Role: role, CSRFToken: p.CSRFToken, ExpiresAt: p.ExpiresAt,
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrUnauthenticated) {
		return SessionInfo{}, fmt.Errorf("auth: me: %w", err)
	}
	return info, err
}

// IsPlatformAdmin reports whether the user is an active platform admin (users.is_platform_admin).
// The api checks it before every /admin operation; the admin definer functions check it again.
func (s *Service) IsPlatformAdmin(ctx context.Context, userID uuid.UUID) (bool, error) {
	var admin bool
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		u, err := authdb.New(tx).GetUserByID(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		admin = u.IsPlatformAdmin && u.Status == userStatusActive
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("auth: platform admin flag: %w", err)
	}
	return admin, nil
}

// HashPassword hashes a new password with the admin hash's argon2id parameters through the
// bounded queue shared with login and registration (a full queue is a *RateLimitedError). The
// platform admin's password reset uses it (ADR-0097).
func (s *Service) HashPassword(ctx context.Context, password string) (string, error) {
	if s.dummy == nil {
		return "", errors.New("auth: BootstrapAdmin has not run")
	}
	if err := s.acquireHash(ctx); err != nil {
		return "", err
	}
	phc, err := crypto.HashPassword(password, s.dummy.Params)
	<-s.hashSem
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return phc, nil
}

// Logout deletes the session and records it.
func (s *Service) Logout(ctx context.Context, p Principal, ip netip.Addr) error {
	err := db.WithTenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		if _, err := authdb.New(tx).DeleteSession(ctx, p.SessionID); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: p.TenantID, ActorUserID: &p.UserID, Action: ActionLogout,
			EntityType: "session", EntityID: p.SessionID.String(), IP: ip,
		})
	})
	if err != nil {
		return fmt.Errorf("auth: logout: %w", err)
	}
	return nil
}

// truncate makes s valid UTF-8 (Postgres rejects anything else) and at most n bytes long.
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary so the column stays valid UTF-8.
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
