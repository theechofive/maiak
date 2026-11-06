package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/audit"
	"github.com/theechofive/maiak/backend/internal/auth/authdb"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/tenants"
)

// Audit actions of OAuth sign-in (ADR-0104). Metadata never carries a token, a code or an email.
const (
	ActionOAuthLogin        = "auth.oauth_login"
	ActionOAuthFailed       = "auth.oauth_failed"
	ActionIdentityLinked    = "auth.identity_linked"
	ActionIdentityUnlinked  = "auth.identity_unlinked"
	ActionAuthDomainChanged = "org.auth_domain_changed"
)

// ProviderGoogle is the provider whose hd claim can join an organization by domain.
const ProviderGoogle = "google"

var (
	// ErrOAuthNotInvited means no user has the identity and nothing allows creating one (no
	// invitation, no claimed domain, registration off).
	ErrOAuthNotInvited = errors.New("auth: identity not linked and not invited")
	// ErrOAuthAccountExists means an account already uses the provider's email. It is never linked
	// automatically (pre-hijacking, ADR-0104): sign in with the password, then link.
	ErrOAuthAccountExists = errors.New("auth: an account with this email exists")
	// ErrOAuthEmailUnusable means the provider's verified email is not a valid login.
	ErrOAuthEmailUnusable = errors.New("auth: the provider's email cannot be used as a login")
	// ErrIdentityInUse means the provider account is linked to another user.
	ErrIdentityInUse = errors.New("auth: identity linked to another user")
	// ErrAlreadyLinked means the user already has an identity of that provider.
	ErrAlreadyLinked = errors.New("auth: an identity of this provider is already linked")
	// ErrIdentityNotFound means the user has no identity of that provider.
	ErrIdentityNotFound = errors.New("auth: identity not found")
	// ErrLastLoginMethod means unlinking would leave the user without a way to sign in.
	ErrLastLoginMethod = errors.New("auth: the last way to sign in cannot be removed")
)

// OAuthIdentity is a verified provider account (internal/oauth checked it).
type OAuthIdentity struct {
	Provider     string
	Subject      string
	Email        string
	HostedDomain string
	DisplayName  string
}

// OAuthLoginInput is a finished OAuth sign-in.
type OAuthLoginInput struct {
	Identity OAuthIdentity
	// InviteHash is the SHA-256 of an invitation token checked at the start of the flow.
	InviteHash []byte
	// RegistrationEnabled allows creating a new organization (MAIAK_REGISTRATION_ENABLED).
	RegistrationEnabled bool
	IP                  netip.Addr
	UserAgent           string
}

// ReserveOAuthAttempt counts an OAuth callback against the login's per-IP limit (ADR-0104). The
// returned function gives the attempt back; the caller calls it when the callback succeeded.
func (s *Service) ReserveOAuthAttempt(ctx context.Context, ip netip.Addr) (func(), error) {
	key := limiterIPKey(ip)
	if ok, wait := s.ipLimit.Reserve(key); !ok {
		s.log.WarnContext(ctx, "oauth callback rate limited")
		return nil, &RateLimitedError{RetryAfter: wait}
	}
	return func() { s.ipLimit.Refund(key) }, nil
}

// RecordOAuthFailure audits a failed OAuth sign-in or link (auth.oauth_failed) and counts it.
// tenantID is the tenant of the user or the session when known; uuid.Nil means the default
// tenant. reason is a short stable code, never provider text.
func (s *Service) RecordOAuthFailure(ctx context.Context, provider, reason string, tenantID uuid.UUID, actor *uuid.UUID, ip netip.Addr) {
	s.oauthLogins.WithLabelValues(provider, reason).Inc()
	if tenantID == uuid.Nil {
		tenantID = s.defaultTenant
	}
	if tenantID == uuid.Nil {
		return
	}
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorUserID: actor, Action: ActionOAuthFailed, EntityType: "user",
			Metadata: map[string]any{"provider": provider, "reason": reason}, IP: ip,
		})
	})
	if err != nil {
		// The answer to the user is the same either way; the failure itself is in the logs.
		s.log.WarnContext(ctx, "could not audit an oauth failure", "reason", reason, "error", err)
	}
}

// oauthFailure is an OAuth sign-in refused for a reason worth an audit row.
type oauthFailure struct {
	err    error
	reason string
	tenant uuid.UUID
	actor  *uuid.UUID
}

func (f *oauthFailure) Error() string { return f.err.Error() }
func (f *oauthFailure) Unwrap() error { return f.err }

func refuse(err error, reason string, tenant uuid.UUID, actor *uuid.UUID) error {
	return &oauthFailure{err: err, reason: reason, tenant: tenant, actor: actor}
}

// OAuthLogin signs a user in with a verified provider identity (ADR-0104). The user is found by
// (provider, subject), never by email. Without a linked user, one is created only with a valid
// invitation, a Google Workspace domain claimed by an organization, or open registration (in this
// order); an email that already has an account answers ErrOAuthAccountExists. Every outcome is
// audited. Errors are domain errors (the caller maps them to a redirect code) or internal ones.
func (s *Service) OAuthLogin(ctx context.Context, in OAuthLoginInput) (LoginResult, error) {
	if s.defaultTenant == uuid.Nil {
		return LoginResult{}, errors.New("auth: BootstrapAdmin has not run")
	}
	provider := in.Identity.Provider
	res, err := s.oauthLogin(ctx, in)
	var f *oauthFailure
	if errors.As(err, &f) {
		s.RecordOAuthFailure(ctx, provider, f.reason, f.tenant, f.actor, in.IP)
		return LoginResult{}, f.err
	}
	if err != nil {
		s.oauthLogins.WithLabelValues(provider, "error").Inc()
		return LoginResult{}, fmt.Errorf("auth: oauth login: %w", err)
	}
	s.oauthLogins.WithLabelValues(provider, "succeeded").Inc()
	return res, nil
}

func (s *Service) oauthLogin(ctx context.Context, in OAuthLoginInput) (LoginResult, error) {
	id := in.Identity
	email, msg := normalizeEmail(id.Email)
	if msg != "" || id.Subject == "" {
		return LoginResult{}, refuse(ErrOAuthEmailUnusable, "email_unusable", uuid.Nil, nil)
	}
	in.Identity.Email = email

	var linked *authdb.GetIdentityRow
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := authdb.New(tx).GetIdentity(ctx, authdb.GetIdentityParams{Provider: id.Provider, Subject: id.Subject})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			linked = &r
		}
		return err
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("get identity: %w", err)
	}
	if linked != nil {
		if linked.Status != userStatusActive {
			return LoginResult{}, refuse(ErrInvalidCredentials, "user_disabled", uuid.Nil, &linked.UserID)
		}
		if len(in.InviteHash) > 0 {
			return s.oauthJoinInvitation(ctx, in, linked.UserID)
		}
		return s.oauthSignIn(ctx, in, linked.UserID)
	}

	existing, err := s.userByEmail(ctx, email)
	if err != nil {
		return LoginResult{}, err
	}
	if existing != nil {
		return LoginResult{}, refuse(ErrOAuthAccountExists, "account_exists", uuid.Nil, nil)
	}
	if len(in.InviteHash) > 0 {
		return s.oauthJoinInvitation(ctx, in, uuid.Nil)
	}
	if id.Provider == ProviderGoogle && id.HostedDomain != "" {
		var tenantID *uuid.UUID
		err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `SELECT tenant_id FROM auth_domain_lookup($1)`, id.HostedDomain).Scan(&tenantID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		})
		if err != nil {
			return LoginResult{}, fmt.Errorf("look up domain: %w", err)
		}
		if tenantID != nil {
			return s.oauthJoinDomain(ctx, in, *tenantID)
		}
	}
	if in.RegistrationEnabled {
		return s.oauthRegister(ctx, in)
	}
	return LoginResult{}, refuse(ErrOAuthNotInvited, "not_invited", uuid.Nil, nil)
}

// oauthSignIn creates a session for a linked user in the tenant a password login would choose.
func (s *Service) oauthSignIn(ctx context.Context, in OAuthLoginInput, userID uuid.UUID) (LoginResult, error) {
	var tenantID *uuid.UUID
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT auth_login_tenant($1)`, userID).Scan(&tenantID)
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("login tenant: %w", err)
	}
	if tenantID == nil {
		// Removed from every organization: an invitation brings the user back.
		return LoginResult{}, refuse(ErrOAuthNotInvited, "no_active_membership", uuid.Nil, &userID)
	}
	var res LoginResult
	err = db.WithTenantTx(ctx, s.pool, *tenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		u, err := q.LockUserForIdentities(ctx, userID)
		if err != nil || u.Status != userStatusActive {
			return refuse(ErrInvalidCredentials, "user_disabled", *tenantID, &userID)
		}
		role, err := q.GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: *tenantID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse(ErrOAuthNotInvited, "no_active_membership", *tenantID, &userID)
		}
		if err != nil {
			return fmt.Errorf("get membership: %w", err)
		}
		t, err := tenants.Get(ctx, tx, *tenantID)
		if errors.Is(err, tenants.ErrNotFound) || (err == nil && t.Status != tenants.StatusActive) {
			return refuse(ErrAccountSuspended, "tenant_suspended", *tenantID, &userID)
		}
		if err != nil {
			return err
		}
		res, err = s.oauthSession(ctx, tx, in, t, userID, role, false, nil)
		return err
	})
	return res, oauthTxErr(*tenantID, &userID, err)
}

// oauthJoinInvitation joins the invitation's organization: a linked user (userID set) like
// AcceptInvitation, a new user (userID nil) created with the identity. The session is bound to the
// invited organization.
func (s *Service) oauthJoinInvitation(ctx context.Context, in OAuthLoginInput, userID uuid.UUID) (LoginResult, error) {
	var actor *uuid.UUID
	if userID != uuid.Nil {
		actor = &userID
	}
	target, err := s.lookupInvitationHash(ctx, in.InviteHash)
	if errors.Is(err, ErrInvitationInvalid) {
		return LoginResult{}, refuse(err, "invitation_invalid", uuid.Nil, actor)
	}
	if err != nil {
		return LoginResult{}, err
	}
	s.beforeJoin()
	var res LoginResult
	err = db.WithTenantTx(ctx, s.pool, target.tenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		created := userID == uuid.Nil
		if created {
			userID = uuidv7.New()
			if err := s.insertOAuthUser(ctx, q, userID, in.Identity); err != nil {
				return err
			}
		} else {
			u, err := q.LockUserForIdentities(ctx, userID)
			if err != nil || u.Status != userStatusActive {
				return refuse(ErrInvalidCredentials, "user_disabled", target.tenantID, &userID)
			}
			if u.IsPlatformAdmin {
				// BootstrapAdmin refuses to start when the admin belongs to another tenant (ADR-0095).
				return refuse(ErrAlreadyMember, "already_member", target.tenantID, &userID)
			}
		}
		role, err := s.joinInvited(ctx, tx, target.tenantID, userID, in.InviteHash, in.IP)
		if err != nil {
			var limit *plans.LimitError
			switch {
			case errors.Is(err, ErrInvitationInvalid):
				return refuse(err, "invitation_invalid", target.tenantID, nilIf(created, userID))
			case errors.Is(err, ErrAlreadyMember):
				return refuse(err, "already_member", target.tenantID, &userID)
			case errors.As(err, &limit):
				return refuse(err, "plan_limit", target.tenantID, nilIf(created, userID))
			}
			return err
		}
		t, err := tenants.Get(ctx, tx, target.tenantID)
		if err != nil {
			return err
		}
		res, err = s.oauthSession(ctx, tx, in, t, userID, role, created, map[string]any{"via": "invitation"})
		return err
	})
	return res, oauthTxErr(target.tenantID, actor, err)
}

// oauthJoinDomain creates the user as a member of the organization that claimed the Google
// Workspace domain, with the domain's default role.
func (s *Service) oauthJoinDomain(ctx context.Context, in OAuthLoginInput, tenantID uuid.UUID) (LoginResult, error) {
	var res LoginResult
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := plans.CheckJoin(ctx, tx, tenantID, plans.ResourceMember); err != nil {
			var limit *plans.LimitError
			if errors.As(err, &limit) {
				return refuse(err, "plan_limit", tenantID, nil)
			}
			if errors.Is(err, tenants.ErrNotFound) {
				return refuse(ErrOAuthNotInvited, "not_invited", uuid.Nil, nil)
			}
			return err
		}
		t, err := tenants.Get(ctx, tx, tenantID)
		if errors.Is(err, tenants.ErrNotFound) || (err == nil && t.Status != tenants.StatusActive) {
			return refuse(ErrOAuthNotInvited, "not_invited", uuid.Nil, nil)
		}
		if err != nil {
			return err
		}
		q := authdb.New(tx)
		role, err := q.ShareAuthDomainByName(ctx, authdb.ShareAuthDomainByNameParams{TenantID: tenantID, Domain: in.Identity.HostedDomain})
		if errors.Is(err, pgx.ErrNoRows) {
			// Released or turned off since the lookup.
			return refuse(ErrOAuthNotInvited, "not_invited", uuid.Nil, nil)
		}
		if err != nil {
			return fmt.Errorf("get domain: %w", err)
		}
		userID := uuidv7.New()
		if err := s.insertOAuthUser(ctx, q, userID, in.Identity); err != nil {
			return err
		}
		if err := q.InsertMembership(ctx, authdb.InsertMembershipParams{TenantID: tenantID, UserID: userID, Role: role}); err != nil {
			return fmt.Errorf("insert membership: %w", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorUserID: &userID, Action: ActionMemberJoined,
			EntityType: "user", EntityID: userID.String(), IP: in.IP,
			Metadata: map[string]any{"via": "auth_domain", "domain": in.Identity.HostedDomain, "role": role},
		}); err != nil {
			return err
		}
		res, err = s.oauthSession(ctx, tx, in, t, userID, role, true, map[string]any{"via": "auth_domain"})
		return err
	})
	return res, oauthTxErr(tenantID, nil, err)
}

// oauthRegister creates a new organization on the free plan with the user as its owner, like
// password registration (ADR-0092), when registration is open.
func (s *Service) oauthRegister(ctx context.Context, in OAuthLoginInput) (LoginResult, error) {
	for attempt := 1; ; attempt++ {
		slug, err := tenantSlug()
		if err != nil {
			return LoginResult{}, err
		}
		tenantID, userID := uuidv7.New(), uuidv7.New()
		var res LoginResult
		err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
			t, err := tenants.Create(ctx, tx, tenantID, slug, tenantName(in.Identity.Email))
			if err != nil {
				return err
			}
			if err := db.SetTenant(ctx, tx, tenantID); err != nil {
				return err
			}
			q := authdb.New(tx)
			if err := s.insertOAuthUser(ctx, q, userID, in.Identity); err != nil {
				return err
			}
			if err := q.InsertMembership(ctx, authdb.InsertMembershipParams{TenantID: tenantID, UserID: userID, Role: RoleOwner}); err != nil {
				return fmt.Errorf("insert membership: %w", err)
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorUserID: &userID, Action: ActionRegister,
				EntityType: "user", EntityID: userID.String(), IP: in.IP,
				Metadata: map[string]any{"tenant_id": tenantID.String(), "provider": in.Identity.Provider},
			}); err != nil {
				return err
			}
			res, err = s.oauthSession(ctx, tx, in, t, userID, RoleOwner, true, map[string]any{"via": "registration"})
			return err
		})
		if errors.Is(err, tenants.ErrSlugTaken) && attempt < slugAttempts {
			continue
		}
		if err == nil {
			s.registrations.WithLabelValues("succeeded").Inc()
		}
		return res, oauthTxErr(uuid.Nil, nil, err)
	}
}

// insertOAuthUser creates a password-less user with the provider's verified email and links the
// identity. An email taken meanwhile is ErrOAuthAccountExists, an identity linked meanwhile is
// ErrIdentityInUse (both roll the transaction back).
func (s *Service) insertOAuthUser(ctx context.Context, q *authdb.Queries, userID uuid.UUID, id OAuthIdentity) error {
	now := s.clock.Now()
	if err := q.InsertOAuthUser(ctx, authdb.InsertOAuthUserParams{
		ID: userID, Email: id.Email, DisplayName: id.DisplayName, VerifiedAt: &now,
	}); err != nil {
		if db.IsUniqueViolation(err, "users_email_key") {
			return refuse(ErrOAuthAccountExists, "account_exists", uuid.Nil, nil)
		}
		return fmt.Errorf("insert user: %w", err)
	}
	if err := q.InsertIdentity(ctx, identityParams(userID, id, now)); err != nil {
		if db.IsUniqueViolation(err, "auth_identities_pkey") {
			return refuse(ErrIdentityInUse, "identity_in_use", uuid.Nil, nil)
		}
		return fmt.Errorf("insert identity: %w", err)
	}
	return nil
}

func identityParams(userID uuid.UUID, id OAuthIdentity, now time.Time) authdb.InsertIdentityParams {
	return authdb.InsertIdentityParams{UserID: userID, Provider: id.Provider, Subject: id.Subject,
		Email: &id.Email, HostedDomain: optional(id.HostedDomain), Now: now}
}

// oauthSession refreshes the identity, audits the sign-in and creates the session, all in the
// caller's tenant transaction.
func (s *Service) oauthSession(ctx context.Context, tx pgx.Tx, in OAuthLoginInput, t tenants.Tenant, userID uuid.UUID, role string,
	created bool, meta map[string]any) (LoginResult, error) {
	q := authdb.New(tx)
	now := s.clock.Now()
	if !created {
		if err := q.TouchIdentity(ctx, authdb.TouchIdentityParams{
			Email: &in.Identity.Email, HostedDomain: optional(in.Identity.HostedDomain), Now: &now,
			Provider: in.Identity.Provider, Subject: in.Identity.Subject,
		}); err != nil {
			return LoginResult{}, fmt.Errorf("touch identity: %w", err)
		}
	}
	u, err := q.GetUserByID(ctx, userID)
	if err != nil {
		return LoginResult{}, fmt.Errorf("get user: %w", err)
	}
	sess, err := s.insertSession(ctx, q, t.ID, userID, in.IP, in.UserAgent)
	if err != nil {
		return LoginResult{}, err
	}
	md := map[string]any{"provider": in.Identity.Provider, "created": created}
	for k, v := range meta {
		md[k] = v
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: t.ID, ActorUserID: &userID, Action: ActionOAuthLogin,
		EntityType: "session", EntityID: sess.id.String(), IP: in.IP, Metadata: md,
	}); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Token: sess.token, SessionInfo: SessionInfo{
		User:   User{ID: u.ID, Login: u.Email, DisplayName: u.DisplayName, IsPlatformAdmin: u.IsPlatformAdmin},
		Tenant: t, Role: role, CSRFToken: sess.csrf, ExpiresAt: sess.expiresAt,
	}}, nil
}

// oauthTxErr passes refusals through and gives refusals without a tenant the transaction's
// tenant for the audit row (the rolled-back transaction could not write it).
func oauthTxErr(tenantID uuid.UUID, actor *uuid.UUID, err error) error {
	var f *oauthFailure
	if errors.As(err, &f) {
		if f.tenant == uuid.Nil && f.reason != "not_invited" {
			f.tenant = tenantID
		}
		if f.actor == nil {
			f.actor = actor
		}
		return f
	}
	return err
}

func nilIf(created bool, id uuid.UUID) *uuid.UUID {
	if created {
		// The user was created in the rolled-back transaction: no row to reference.
		return nil
	}
	return &id
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// LinkIdentity adds a verified provider identity to the signed-in user (intent link, ADR-0104).
// Linking the identity the user already has is a no-op; one linked to someone else is
// ErrIdentityInUse; a second identity of the same provider is ErrAlreadyLinked. When the provider's
// verified email is the user's login, the email counts as verified from now on.
func (s *Service) LinkIdentity(ctx context.Context, p Principal, id OAuthIdentity, ip netip.Addr) error {
	email, msg := normalizeEmail(id.Email)
	if msg != "" || id.Subject == "" {
		s.RecordOAuthFailure(ctx, id.Provider, "email_unusable", p.TenantID, &p.UserID, ip)
		return ErrOAuthEmailUnusable
	}
	id.Email = email
	now := s.clock.Now()
	reason := ""
	err := db.WithTenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		u, err := q.LockUserForIdentities(ctx, p.UserID)
		if err != nil || u.Status != userStatusActive {
			return ErrUnauthenticated
		}
		cur, err := q.GetIdentity(ctx, authdb.GetIdentityParams{Provider: id.Provider, Subject: id.Subject})
		switch {
		case err == nil && cur.UserID == p.UserID:
			return q.TouchIdentity(ctx, authdb.TouchIdentityParams{Email: &id.Email, HostedDomain: optional(id.HostedDomain),
				Now: &now, Provider: id.Provider, Subject: id.Subject})
		case err == nil:
			reason = "identity_in_use"
			return ErrIdentityInUse
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("get identity: %w", err)
		}
		mine, err := q.ListIdentities(ctx, p.UserID)
		if err != nil {
			return fmt.Errorf("list identities: %w", err)
		}
		for _, m := range mine {
			if m.Provider == id.Provider {
				reason = "already_linked"
				return ErrAlreadyLinked
			}
		}
		if err := q.InsertIdentity(ctx, identityParams(p.UserID, id, now)); err != nil {
			if db.IsUniqueViolation(err, "auth_identities_pkey") {
				reason = "identity_in_use"
				return ErrIdentityInUse
			}
			return fmt.Errorf("insert identity: %w", err)
		}
		if strings.EqualFold(u.Email, id.Email) {
			if err := q.MarkEmailVerified(ctx, authdb.MarkEmailVerifiedParams{ID: p.UserID, VerifiedAt: &now}); err != nil {
				return fmt.Errorf("mark email verified: %w", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: p.TenantID, ActorUserID: &p.UserID, Action: ActionIdentityLinked,
			EntityType: "user", EntityID: p.UserID.String(), IP: ip,
			Metadata: map[string]any{"provider": id.Provider},
		})
	})
	switch {
	case err == nil:
		s.oauthLogins.WithLabelValues(id.Provider, "linked").Inc()
		return nil
	case reason != "":
		s.RecordOAuthFailure(ctx, id.Provider, reason, p.TenantID, &p.UserID, ip)
		return err
	case errors.Is(err, ErrUnauthenticated):
		return err
	}
	return fmt.Errorf("auth: link identity: %w", err)
}

// Identity is a linked provider identity as the user sees it.
type Identity struct {
	Provider    string
	Email       string
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

// SignInMethods is what the profile page shows.
type SignInMethods struct {
	Identities []Identity
	// PasswordSet reports whether the user has a password.
	PasswordSet bool
	// PasswordLoginEnabled reports whether the user may sign in with it.
	PasswordLoginEnabled bool
}

// ListIdentities returns the signed-in user's sign-in methods.
func (s *Service) ListIdentities(ctx context.Context, p Principal) (SignInMethods, error) {
	var out SignInMethods
	err := db.WithTenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		u, err := q.GetUserSignIn(ctx, p.UserID)
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		out.PasswordSet = u.PasswordHash != nil
		out.PasswordLoginEnabled = s.passwordAllowed(u.Email)
		rows, err := q.ListIdentities(ctx, p.UserID)
		if err != nil {
			return err
		}
		out.Identities = make([]Identity, len(rows))
		for i, r := range rows {
			var email string
			if r.Email != nil {
				email = *r.Email
			}
			out.Identities[i] = Identity{Provider: r.Provider, Email: email, CreatedAt: r.CreatedAt, LastLoginAt: r.LastLoginAt}
		}
		return nil
	})
	if err != nil {
		return SignInMethods{}, fmt.Errorf("auth: list identities: %w", err)
	}
	return out, nil
}

// passwordAllowed reports whether a user with this login may sign in with a password.
func (s *Service) passwordAllowed(login string) bool {
	return !s.passwordLoginDisabled || normalizeLogin(login) == s.adminLogin
}

// UnlinkIdentity removes the user's identity of a provider while another way to sign in remains:
// a password the user may use (password login on, or the admin login) or another identity.
func (s *Service) UnlinkIdentity(ctx context.Context, p Principal, provider string, ip netip.Addr) error {
	err := db.WithTenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		u, err := q.LockUserForIdentities(ctx, p.UserID)
		if err != nil || u.Status != userStatusActive {
			return ErrUnauthenticated
		}
		mine, err := q.ListIdentities(ctx, p.UserID)
		if err != nil {
			return fmt.Errorf("list identities: %w", err)
		}
		found := false
		for _, m := range mine {
			found = found || m.Provider == provider
		}
		if !found {
			return ErrIdentityNotFound
		}
		password := u.PasswordHash != nil && s.passwordAllowed(u.Email)
		if len(mine) == 1 && !password {
			return ErrLastLoginMethod
		}
		if _, err := q.DeleteIdentity(ctx, authdb.DeleteIdentityParams{UserID: p.UserID, Provider: provider}); err != nil {
			return fmt.Errorf("delete identity: %w", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: p.TenantID, ActorUserID: &p.UserID, Action: ActionIdentityUnlinked,
			EntityType: "user", EntityID: p.UserID.String(), IP: ip,
			Metadata: map[string]any{"provider": provider},
		})
	})
	switch {
	case err == nil, errors.Is(err, ErrUnauthenticated), errors.Is(err, ErrIdentityNotFound), errors.Is(err, ErrLastLoginMethod):
		return err
	}
	return fmt.Errorf("auth: unlink identity: %w", err)
}
