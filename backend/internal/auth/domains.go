package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/audit"
	"github.com/theechofive/maiak/backend/internal/auth/authdb"
	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
)

// maxAuthDomains is how many sign-in domains one organization may claim.
const maxAuthDomains = 10

// domainPattern is the org_auth_domains.domain check: lowercase labels, at least one dot, a
// top-level label that starts with a letter.
var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

var (
	// ErrAuthDomainNotFound means no sign-in domain with that id in the organization.
	ErrAuthDomainNotFound = errors.New("auth: sign-in domain not found")
	// ErrDomainNotVerified means the owner's linked Google account did not last sign in with that
	// Workspace domain (the proof of control, ADR-0104).
	ErrDomainNotVerified = errors.New("auth: domain not verified by a Google Workspace sign-in")
	// ErrDomainTaken means another organization claimed the domain.
	ErrDomainTaken = errors.New("auth: domain claimed by another organization")
)

// AuthDomain is a Google Workspace domain claimed by an organization.
type AuthDomain struct {
	ID          uuid.UUID
	Domain      string
	DefaultRole string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ListAuthDomains returns the organization's sign-in domains.
func (s *Service) ListAuthDomains(ctx context.Context, tenantID uuid.UUID) ([]AuthDomain, error) {
	var out []AuthDomain
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := authdb.New(tx).ListAuthDomains(ctx, tenantID)
		if err != nil {
			return err
		}
		out = make([]AuthDomain, len(rows))
		for i, r := range rows {
			out[i] = AuthDomain(r)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("auth: list sign-in domains: %w", err)
	}
	return out, nil
}

// CreateAuthDomain claims a domain for the actor's organization (owner only). The actor's linked
// Google identity must have signed in last with that hd.
func (s *Service) CreateAuthDomain(ctx context.Context, a Actor, domain, defaultRole string, enabled bool) (AuthDomain, error) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if len(domain) < 3 || len(domain) > 253 || !domainPattern.MatchString(domain) {
		var verr validation.Error
		verr.Add("domain", "must be a domain name such as example.com")
		return AuthDomain{}, verr.Err()
	}
	if defaultRole == "" {
		defaultRole = RoleViewer
	}
	now := s.clock.Now()
	d := AuthDomain{ID: uuidv7.New(), Domain: domain, DefaultRole: defaultRole, Enabled: enabled, CreatedAt: now, UpdatedAt: now}
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		if err := requireOwner(ctx, tx, a); err != nil {
			return err
		}
		q := authdb.New(tx)
		proven, err := q.HasGoogleDomain(ctx, authdb.HasGoogleDomainParams{UserID: a.UserID, Domain: &domain})
		if err != nil {
			return fmt.Errorf("check domain proof: %w", err)
		}
		if !proven {
			return ErrDomainNotVerified
		}
		n, err := q.CountAuthDomains(ctx, a.TenantID)
		if err != nil {
			return fmt.Errorf("count domains: %w", err)
		}
		if n >= maxAuthDomains {
			var verr validation.Error
			verr.Add("domain", fmt.Sprintf("an organization may claim at most %d domains", maxAuthDomains))
			return verr.Err()
		}
		if err := q.InsertAuthDomain(ctx, authdb.InsertAuthDomainParams{
			ID: d.ID, TenantID: a.TenantID, Domain: domain, DefaultRole: defaultRole, Enabled: enabled, CreatedBy: a.UserID, Now: now,
		}); err != nil {
			if db.IsUniqueViolation(err, "org_auth_domains_domain_key") {
				return ErrDomainTaken
			}
			return fmt.Errorf("insert domain: %w", err)
		}
		return s.auditDomain(ctx, tx, a, d, "created")
	})
	if err != nil {
		return AuthDomain{}, wrapDomainErr("create", err)
	}
	return d, nil
}

// UpdateAuthDomain changes a domain's default role or switches it on or off (owner only).
func (s *Service) UpdateAuthDomain(ctx context.Context, a Actor, id uuid.UUID, defaultRole *string, enabled *bool) (AuthDomain, error) {
	var d AuthDomain
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		if err := requireOwner(ctx, tx, a); err != nil {
			return err
		}
		q := authdb.New(tx)
		cur, err := q.LockAuthDomain(ctx, authdb.LockAuthDomainParams{TenantID: a.TenantID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAuthDomainNotFound
		}
		if err != nil {
			return fmt.Errorf("lock domain: %w", err)
		}
		d = AuthDomain(cur)
		if defaultRole != nil {
			d.DefaultRole = *defaultRole
		}
		if enabled != nil {
			d.Enabled = *enabled
		}
		if d.DefaultRole == cur.DefaultRole && d.Enabled == cur.Enabled {
			return nil
		}
		d.UpdatedAt = s.clock.Now()
		if err := q.UpdateAuthDomain(ctx, authdb.UpdateAuthDomainParams{
			TenantID: a.TenantID, ID: id, DefaultRole: d.DefaultRole, Enabled: d.Enabled, Now: d.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("update domain: %w", err)
		}
		return s.auditDomain(ctx, tx, a, d, "updated")
	})
	if err != nil {
		return AuthDomain{}, wrapDomainErr("update", err)
	}
	return d, nil
}

// DeleteAuthDomain releases a domain (owner only); members who joined through it stay.
func (s *Service) DeleteAuthDomain(ctx context.Context, a Actor, id uuid.UUID) error {
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		if err := requireOwner(ctx, tx, a); err != nil {
			return err
		}
		q := authdb.New(tx)
		cur, err := q.LockAuthDomain(ctx, authdb.LockAuthDomainParams{TenantID: a.TenantID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAuthDomainNotFound
		}
		if err != nil {
			return fmt.Errorf("lock domain: %w", err)
		}
		if _, err := q.DeleteAuthDomain(ctx, authdb.DeleteAuthDomainParams{TenantID: a.TenantID, ID: id}); err != nil {
			return fmt.Errorf("delete domain: %w", err)
		}
		return s.auditDomain(ctx, tx, a, AuthDomain(cur), "deleted")
	})
	return wrapDomainErr("delete", err)
}

// requireOwner re-reads the actor's role in the transaction (the operation table checked it at the
// start of the request).
func requireOwner(ctx context.Context, tx pgx.Tx, a Actor) error {
	role, err := authdb.New(tx).GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: a.TenantID, UserID: a.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthenticated
	}
	if err != nil {
		return fmt.Errorf("get actor role: %w", err)
	}
	if role != RoleOwner {
		return authz.ErrForbidden
	}
	return nil
}

func (s *Service) auditDomain(ctx context.Context, tx pgx.Tx, a Actor, d AuthDomain, change string) error {
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: a.TenantID, ActorUserID: &a.UserID, Action: ActionAuthDomainChanged,
		EntityType: "auth_domain", EntityID: d.ID.String(), IP: a.IP,
		Metadata: map[string]any{"change": change, "domain": d.Domain, "default_role": d.DefaultRole, "enabled": d.Enabled},
	})
}

func wrapDomainErr(op string, err error) error {
	var verr *validation.Error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrAuthDomainNotFound), errors.Is(err, ErrDomainNotVerified), errors.Is(err, ErrDomainTaken),
		errors.Is(err, authz.ErrForbidden), errors.Is(err, ErrUnauthenticated), errors.As(err, &verr):
		return err
	}
	return fmt.Errorf("auth: %s sign-in domain: %w", op, err)
}
