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
	// maxSlugBase keeps "<base>-<suffix>" well inside the tenants.slug limit of 63 characters.
	maxSlugBase   = 40
	slugSuffixLen = 6
	// slugAttempts bounds retries after a slug collision (30 random bits per suffix).
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
	email, err := validateRegistration(in.Email, in.Password)
	if err != nil {
		s.registrations.WithLabelValues("invalid").Inc()
		return LoginResult{}, err
	}
	ipKey := limiterIPKey(in.IP)
	if ok, wait := s.ipLimit.Reserve(ipKey); !ok {
		return LoginResult{}, s.registrationRateLimited(ctx, &RateLimitedError{RetryAfter: wait})
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
	slug, err := tenantSlug(email)
	if err != nil {
		return LoginResult{}, err
	}
	tenantID, userID := uuidv7.New(), uuidv7.New()
	var res LoginResult
	// WithTx plus SetTenant (not WithTenantTx): the tenant only exists once this transaction
	// commits, and a rolled-back attempt must not tag the request logs with its id.
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		t, err := tenants.Create(ctx, tx, tenantID, slug, tenantName(email), plans.KeyFree)
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

// tenantSlug derives a unique-enough slug from the local part of a normalised email: runs of
// characters outside [a-z0-9] become one dash, plus a random suffix ("john-smith-k3x9qa").
func tenantSlug(email string) (string, error) {
	local, _, _ := strings.Cut(email, "@")
	var b strings.Builder
	for _, c := range local {
		if b.Len() >= maxSlugBase {
			break
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "workspace"
	}
	raw := make([]byte, 5)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("random slug suffix: %w", err)
	}
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))[:slugSuffixLen]
	return base + "-" + suffix, nil
}

// tenantName is the email's domain, or a generic name when the domain is too long.
func tenantName(email string) string {
	_, domain, _ := strings.Cut(email, "@")
	if domain == "" || len(domain) > maxTenantName {
		return defaultTenantName
	}
	return domain
}
