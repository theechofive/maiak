package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/audit"
	"github.com/theechofive/maiak/backend/internal/auth/authdb"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
	"github.com/theechofive/maiak/backend/internal/tenants"
)

// Registration policy (P-02, ADR-0092, ADR-0009 amendment 2026-10-21).
const (
	minEmailLen      = 3
	maxEmailLen      = 254
	minPasswordRunes = 12
	maxPasswordRunes = 128
	// Registered tenants get "t-" plus slugRandomLen random base32 characters as their slug.
	slugPrefix    = "t-"
	slugRandomLen = 10
	// slugAttempts bounds retries after a slug collision (50 random bits per slug).
	slugAttempts = 3
	// maxTenantName is the tenants.name limit; a longer domain falls back to defaultTenantName.
	maxTenantName     = 200
	defaultTenantName = "Workspace"
)

// ErrEmailTaken means a user with the email exists. Registration without verification cannot
// hide this (ADR-0092): the answer is a 409 with a neutral message.
var ErrEmailTaken = errors.New("auth: email already registered")

// RegisterInput is a self-service registration.
type RegisterInput struct {
	Email     string
	Password  string
	IP        netip.Addr
	UserAgent string
	// InviteToken, when set, makes the user a member of the inviting organization instead of
	// creating one (ADR-0102). The caller allows it even when registration is turned off.
	InviteToken string
}

// Register creates a tenant on the free plan, the user as its owner, an audit row and a session
// in one transaction, and returns the session like Login. A duplicate email (also under
// concurrency, decided by the unique constraint) rolls everything back and returns
// ErrEmailTaken. Policy failures are *validation.Error. Each attempt that reaches the password
// hash counts against the per-IP login limiter, a success included, and hashing shares the
// login's bounded queue, so a flood is answered with *RateLimitedError.
func (s *Service) Register(ctx context.Context, in RegisterInput) (LoginResult, error) {
	if s.dummy == nil {
		return LoginResult{}, errors.New("auth: BootstrapAdmin has not run")
	}
	if s.passwordLoginDisabled {
		return LoginResult{}, ErrPasswordLoginDisabled
	}
	email, err := validateRegistration(in.Email, in.Password)
	if err != nil {
		s.registrations.WithLabelValues("invalid").Inc()
		return LoginResult{}, err
	}
	ipKey := limiterIPKey(in.IP)
	if ok, wait := s.ipLimit.Reserve(ipKey); !ok {
		return LoginResult{}, s.registrationRateLimited(ctx, &RateLimitedError{RetryAfter: wait})
	}
	if in.InviteToken != "" {
		return s.registerInvited(ctx, email, in, ipKey)
	}
	if err := s.acquireHash(ctx); err != nil {
		s.ipLimit.Refund(ipKey)
		return LoginResult{}, s.registrationRateLimited(ctx, err)
	}
	// Same function and parameters as the admin hash (and the dummy), so every stored hash
	// costs the same to verify.
	phc, err := crypto.HashPassword(in.Password, s.dummy.Params)
	<-s.hashSem
	if err != nil {
		s.ipLimit.Refund(ipKey)
		return LoginResult{}, fmt.Errorf("auth: register: hash password: %w", err)
	}

	for attempt := 1; ; attempt++ {
		res, err := s.createAccount(ctx, email, phc, in)
		switch {
		case err == nil:
			s.registrations.WithLabelValues("succeeded").Inc()
			s.log.InfoContext(ctx, "account registered", "user_id", res.User.ID, "tenant_id", res.Tenant.ID)
			return res, nil
		case errors.Is(err, ErrEmailTaken):
			s.registrations.WithLabelValues("email_taken").Inc()
			return LoginResult{}, ErrEmailTaken
		case errors.Is(err, tenants.ErrSlugTaken) && attempt < slugAttempts:
			continue
		default:
			s.ipLimit.Refund(ipKey)
			return LoginResult{}, fmt.Errorf("auth: register: %w", err)
		}
	}
}

func (s *Service) createAccount(ctx context.Context, email, passwordHash string, in RegisterInput) (LoginResult, error) {
	slug, err := tenantSlug()
	if err != nil {
		return LoginResult{}, err
	}
	tenantID, userID := uuidv7.New(), uuidv7.New()
	var res LoginResult
	// WithTx plus SetTenant (not WithTenantTx): the tenant only exists once this transaction
	// commits, and a rolled-back attempt must not tag the request logs with its id.
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		t, err := tenants.Create(ctx, tx, tenantID, slug, tenantName(email))
		if err != nil {
			return err
		}
		if err := db.SetTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		q := authdb.New(tx)
		if err := q.InsertUser(ctx, authdb.InsertUserParams{ID: userID, Email: email, PasswordHash: &passwordHash}); err != nil {
			if db.IsUniqueViolation(err, "users_email_key") {
				return ErrEmailTaken
			}
			return fmt.Errorf("insert user: %w", err)
		}
		if err := q.InsertMembership(ctx, authdb.InsertMembershipParams{TenantID: tenantID, UserID: userID, Role: RoleOwner}); err != nil {
			return fmt.Errorf("insert membership: %w", err)
		}
		// Only the tenant id: never the email (it is the login) and never the password or hash.
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorUserID: &userID, Action: ActionRegister,
			EntityType: "user", EntityID: userID.String(), IP: in.IP,
			Metadata: map[string]any{"tenant_id": tenantID.String()},
		}); err != nil {
			return err
		}
		sess, err := s.insertSession(ctx, q, tenantID, userID, in.IP, in.UserAgent)
		if err != nil {
			return err
		}
		res = LoginResult{Token: sess.token, SessionInfo: SessionInfo{
			User: User{ID: userID, Login: email}, Tenant: t, Role: RoleOwner, CSRFToken: sess.csrf, ExpiresAt: sess.expiresAt,
		}}
		return nil
	})
	return res, err
}

// registerInvited joins the invited organization with the email and password given (T-01, T-12):
// a new email gets a new user; an email that already has an account must come with that account's
// password, checked like a login (one argon2 operation either way, the same generic
// ErrInvalidCredentials, the invitation untouched on a wrong password). Every attempt that does
// not succeed counts against the per-IP invitation limit and the per-invitation limit; a wrong
// password also counts against the login's per-login limit.
func (s *Service) registerInvited(ctx context.Context, email string, in RegisterInput, ipKey string) (LoginResult, error) {
	inviteKey, err := s.reserveInvite(ctx, in.IP)
	if err != nil {
		s.ipLimit.Refund(ipKey)
		return LoginResult{}, s.registrationRateLimited(ctx, err)
	}
	target, err := s.lookupInvitation(ctx, in.InviteToken)
	if err != nil {
		s.registrations.WithLabelValues("invalid_invitation").Inc()
		return LoginResult{}, err
	}
	perInvite := "invitation:" + target.invitationID.String()
	if ok, wait := s.inviteLimit.Reserve(perInvite); !ok {
		s.inviteLimit.Refund(inviteKey)
		return LoginResult{}, s.registrationRateLimited(ctx, &RateLimitedError{RetryAfter: wait})
	}
	succeeded := func() {
		s.inviteLimit.Refund(inviteKey)
		s.inviteLimit.Refund(perInvite)
	}
	s.beforeJoin()

	existing, err := s.userByEmail(ctx, email)
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: register: %w", err)
	}
	var res LoginResult
	if existing != nil {
		res, err = s.joinExistingAccount(ctx, *existing, email, target, in)
	} else {
		res, err = s.joinNewAccount(ctx, email, target, in)
	}
	var limit *plans.LimitError
	switch {
	case err == nil:
		succeeded()
		s.registrations.WithLabelValues("succeeded").Inc()
		s.log.InfoContext(ctx, "account joined through an invitation", "user_id", res.User.ID, "tenant_id", res.Tenant.ID,
			"existing_account", existing != nil)
		return res, nil
	case errors.Is(err, ErrEmailTaken):
		// Only when the email was registered between the check and the insert.
		s.registrations.WithLabelValues("email_taken").Inc()
		return LoginResult{}, ErrEmailTaken
	case errors.Is(err, ErrInvalidCredentials):
		s.registrations.WithLabelValues("invalid_credentials").Inc()
		return LoginResult{}, err
	case errors.Is(err, ErrInvitationInvalid):
		// Used, revoked or expired, or the tenant suspended, between the lookup and the transaction.
		s.registrations.WithLabelValues("invalid_invitation").Inc()
		return LoginResult{}, err
	case errors.Is(err, ErrRateLimited):
		s.registrations.WithLabelValues("rate_limited").Inc()
		return LoginResult{}, err
	case errors.Is(err, ErrAlreadyMember), errors.As(err, &limit):
		succeeded()
		return LoginResult{}, err
	}
	succeeded()
	s.ipLimit.Refund(ipKey)
	return LoginResult{}, fmt.Errorf("auth: register: %w", err)
}

// userByEmail returns the user with the login, or nil.
func (s *Service) userByEmail(ctx context.Context, email string) (*authdb.GetUserByEmailRow, error) {
	var row *authdb.GetUserByEmailRow
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := authdb.New(tx).GetUserByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			row = &r
		}
		return err
	})
	return row, err
}

func (s *Service) joinNewAccount(ctx context.Context, email string, target invitationTarget, in RegisterInput) (LoginResult, error) {
	if err := s.acquireHash(ctx); err != nil {
		return LoginResult{}, err
	}
	// Same function and parameters as the admin hash (and the dummy), so every stored hash
	// costs the same to verify.
	phc, err := crypto.HashPassword(in.Password, s.dummy.Params)
	<-s.hashSem
	if err != nil {
		return LoginResult{}, fmt.Errorf("hash password: %w", err)
	}
	return s.createInvitedAccount(ctx, email, phc, target.tenantID, in)
}

// joinExistingAccount verifies the account's password like a login and, when it is right, joins
// the account to the invited organization and creates a session there, all in one transaction
// that re-checks the credential (ADR-0098). A wrong password leaves the invitation open.
func (s *Service) joinExistingAccount(ctx context.Context, row authdb.GetUserByEmailRow, email string, target invitationTarget, in RegisterInput) (LoginResult, error) {
	loginKey := normalizeLogin(email)
	if ok, wait := s.loginLimit.Reserve(loginKey); !ok {
		return LoginResult{}, &RateLimitedError{RetryAfter: wait}
	}
	hash := s.dummy
	if row.PasswordHash != nil {
		if h, perr := crypto.ParseArgon2idHash(*row.PasswordHash); perr == nil {
			hash = h
		}
	}
	if err := s.acquireHash(ctx); err != nil {
		s.loginLimit.Refund(loginKey)
		return LoginResult{}, err
	}
	match := hash.Verify(in.Password)
	<-s.hashSem
	if !match || hash == s.dummy || row.Status != userStatusActive {
		s.logins.WithLabelValues("failed").Inc()
		if err := s.auditFailure(ctx, target.tenantID, &row.ID, in.IP, "invitation_invalid_credentials"); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	cred := credential{hash: *row.PasswordHash, updatedAt: row.UpdatedAt}
	var res LoginResult
	err := db.WithTenantTx(ctx, s.pool, target.tenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		if err := recheckCredential(ctx, q, row.ID, cred); err != nil {
			return ErrInvalidCredentials
		}
		if row.IsPlatformAdmin {
			// BootstrapAdmin refuses to start when the admin belongs to another tenant (ADR-0095).
			return ErrAlreadyMember
		}
		role, err := s.joinInvited(ctx, tx, target.tenantID, row.ID, hashToken(in.InviteToken), in.IP)
		if err != nil {
			return err
		}
		t, err := tenants.Get(ctx, tx, target.tenantID)
		if err != nil {
			return err
		}
		sess, err := s.insertSession(ctx, q, target.tenantID, row.ID, in.IP, in.UserAgent)
		if err != nil {
			return err
		}
		res = LoginResult{Token: sess.token, SessionInfo: SessionInfo{
			User: User{ID: row.ID, Login: row.Email, DisplayName: row.DisplayName}, Tenant: t, Role: role,
			CSRFToken: sess.csrf, ExpiresAt: sess.expiresAt,
		}}
		return nil
	})
	if err == nil {
		s.loginLimit.Reset(loginKey)
	}
	return res, err
}

// createInvitedAccount creates the user and its membership in the inviting organization (no
// tenant of its own) and a session there, in one transaction bound to that tenant.
func (s *Service) createInvitedAccount(ctx context.Context, email, passwordHash string, tenantID uuid.UUID, in RegisterInput) (LoginResult, error) {
	userID := uuidv7.New()
	var res LoginResult
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		if err := q.InsertUser(ctx, authdb.InsertUserParams{ID: userID, Email: email, PasswordHash: &passwordHash}); err != nil {
			if db.IsUniqueViolation(err, "users_email_key") {
				return ErrEmailTaken
			}
			return fmt.Errorf("insert user: %w", err)
		}
		role, err := s.joinInvited(ctx, tx, tenantID, userID, hashToken(in.InviteToken), in.IP)
		if err != nil {
			return err
		}
		t, err := tenants.Get(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if t.Status != tenants.StatusActive {
			return ErrInvitationInvalid
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorUserID: &userID, Action: ActionRegister,
			EntityType: "user", EntityID: userID.String(), IP: in.IP,
			Metadata: map[string]any{"tenant_id": tenantID.String(), "invited": true},
		}); err != nil {
			return err
		}
		sess, err := s.insertSession(ctx, q, tenantID, userID, in.IP, in.UserAgent)
		if err != nil {
			return err
		}
		res = LoginResult{Token: sess.token, SessionInfo: SessionInfo{
			User: User{ID: userID, Login: email}, Tenant: t, Role: role, CSRFToken: sess.csrf, ExpiresAt: sess.expiresAt,
		}}
		return nil
	})
	return res, err
}

func (s *Service) registrationRateLimited(ctx context.Context, err error) error {
	var rl *RateLimitedError
	if errors.As(err, &rl) {
		s.registrations.WithLabelValues("rate_limited").Inc()
		// Not audited, like the login limit.
		s.log.WarnContext(ctx, "registration rate limited")
	}
	return err
}

// validateRegistration applies the email and password policy and returns the normalised email.
func validateRegistration(rawEmail, password string) (string, error) {
	var verr validation.Error
	email, msg := normalizeEmail(rawEmail)
	if msg != "" {
		verr.Add("email", msg)
	}
	switch n := utf8.RuneCountInString(password); {
	case n < minPasswordRunes:
		verr.Add("password", fmt.Sprintf("must be at least %d characters", minPasswordRunes))
	case n > maxPasswordRunes:
		verr.Add("password", fmt.Sprintf("must be at most %d characters", maxPasswordRunes))
	case msg == "" && strings.EqualFold(password, email):
		verr.Add("password", "must not be the same as the email")
	}
	return email, verr.Err()
}

// normalizeEmail trims and lowercases an email and checks the registration policy; it returns
// a user-facing message when the email is rejected. The alphabet is the login alphabet
// (config.NormalizeLogin) so that every registered email can be used to log in.
func normalizeEmail(raw string) (string, string) {
	s := strings.TrimSpace(raw)
	if len(s) < minEmailLen || len(s) > maxEmailLen {
		return "", fmt.Sprintf("must be %d-%d characters", minEmailLen, maxEmailLen)
	}
	// Checked before lowercasing: strings.ToLower maps some non-ASCII runes (the Kelvin sign) to ASCII.
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._+-@", c) {
			return "", "may contain only letters a-z, digits and . _ + - @"
		}
	}
	s = strings.ToLower(s)
	local, domain, ok := strings.Cut(s, "@")
	if !ok || strings.Contains(domain, "@") {
		return "", "must contain exactly one @"
	}
	if local == "" {
		return "", "must have a name before the @"
	}
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", "must have a domain such as example.com after the @"
	}
	return s, ""
}

// tenantSlug returns a random slug ("t-" and 10 base32 characters, 50 random bits). It never
// carries any part of the email: the slug shows up in URLs and logs (ADR-0095), and it can never
// be the default tenant's slug.
func tenantSlug() (string, error) {
	raw := make([]byte, 7)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("random slug: %w", err)
	}
	return slugPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))[:slugRandomLen], nil
}

// tenantName is the email's domain, or a generic name when the domain is too long.
func tenantName(email string) string {
	_, domain, _ := strings.Cut(email, "@")
	if domain == "" || len(domain) > maxTenantName {
		return defaultTenantName
	}
	return domain
}
