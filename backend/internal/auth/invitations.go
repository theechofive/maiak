package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/audit"
	"github.com/theechofive/maiak/backend/internal/auth/authdb"
	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
	"github.com/theechofive/maiak/backend/internal/tenants"
)

// InvitationTTL is how long an invitation link works (ADR-0102).
const InvitationTTL = 7 * 24 * time.Hour

const maxInvitationLabel = 80

// Invitation states as the API reports them.
const (
	InvitationOpen     = "open"
	InvitationAccepted = "accepted"
	InvitationRevoked  = "revoked"
	InvitationExpired  = "expired"
)

var (
	// ErrInvitationInvalid is the one answer for an unknown, expired, used or revoked token
	// (and for an organization that is suspended or deleted), so a token reveals nothing.
	ErrInvitationInvalid = errors.New("auth: invitation invalid")
	// ErrInvitationNotFound means no invitation with that id in the organization.
	ErrInvitationNotFound = errors.New("auth: invitation not found")
	// ErrInvitationAccepted means the invitation was already used; remove the member instead.
	ErrInvitationAccepted = errors.New("auth: invitation already accepted")
	// ErrAlreadyMember means the user already belongs to the inviting organization, or is the
	// platform admin, who must stay a member of the default tenant only (ADR-0095).
	ErrAlreadyMember = errors.New("auth: already a member")
)

// Invitation is an invitation as admins see it; the token is never part of it.
type Invitation struct {
	ID         uuid.UUID
	Role       string
	Label      string
	Status     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	CreatedBy  string
	AcceptedBy *string
	AcceptedAt *time.Time
	RevokedAt  *time.Time
}

// InvitationPreview is what the link page shows before accepting.
type InvitationPreview struct {
	OrganizationName string
	Role             string
	ExpiresAt        time.Time
}

func invitationStatus(now, expires time.Time, accepted, revoked *time.Time) string {
	switch {
	case accepted != nil:
		return InvitationAccepted
	case revoked != nil:
		return InvitationRevoked
	case !now.Before(expires):
		return InvitationExpired
	}
	return InvitationOpen
}

// CreateInvitation creates an invitation (admin or owner, role admin, member or viewer) and
// returns it with its token, which is shown once and never stored (only its SHA-256). Open
// invitations count toward max_members, checked under the tenant row lock.
func (s *Service) CreateInvitation(ctx context.Context, a Actor, role, label string) (Invitation, string, error) {
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > maxInvitationLabel {
		var verr validation.Error
		verr.Add("label", fmt.Sprintf("must be at most %d characters", maxInvitationLabel))
		return Invitation{}, "", verr.Err()
	}
	token, err := newToken()
	if err != nil {
		return Invitation{}, "", err
	}
	now := s.clock.Now()
	inv := Invitation{ID: uuidv7.New(), Role: role, Label: label, Status: InvitationOpen, CreatedAt: now, ExpiresAt: now.Add(InvitationTTL)}
	err = db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		// CheckCreate takes the tenant row lock first, like every member change.
		if err := plans.CheckCreate(ctx, tx, a.TenantID, plans.ResourceMember); err != nil {
			return err
		}
		q := authdb.New(tx)
		role, err := q.GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: a.TenantID, UserID: a.UserID})
		if err != nil {
			return ErrUnauthenticated
		}
		if !authz.CanInvite(authz.Actor{UserID: a.UserID, Role: authz.Role(role)}, authz.Role(inv.Role)) {
			return authz.ErrForbidden
		}
		u, err := q.GetUserByID(ctx, a.UserID)
		if err != nil {
			return fmt.Errorf("get creator: %w", err)
		}
		inv.CreatedBy = u.Email
		if err := q.InsertInvitation(ctx, authdb.InsertInvitationParams{
			ID: inv.ID, TenantID: a.TenantID, Role: inv.Role, Label: inv.Label, TokenHash: hashToken(token),
			CreatedBy: a.UserID, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt,
		}); err != nil {
			return fmt.Errorf("insert invitation: %w", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: a.TenantID, ActorUserID: &a.UserID, Action: ActionMemberInvited,
			EntityType: "invitation", EntityID: inv.ID.String(), IP: a.IP,
			Metadata: map[string]any{"role": inv.Role, "expires_at": inv.ExpiresAt.UTC().Format(time.RFC3339)},
		})
	})
	if err != nil {
		var limit *plans.LimitError
		if errors.As(err, &limit) || errors.Is(err, authz.ErrForbidden) || errors.Is(err, ErrUnauthenticated) {
			return Invitation{}, "", err
		}
		return Invitation{}, "", fmt.Errorf("auth: create invitation: %w", err)
	}
	return inv, token, nil
}

// ListInvitations returns the newest 200 invitations of the organization.
func (s *Service) ListInvitations(ctx context.Context, tenantID uuid.UUID) ([]Invitation, error) {
	now := s.clock.Now()
	var out []Invitation
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := authdb.New(tx).ListInvitations(ctx, tenantID)
		if err != nil {
			return err
		}
		out = make([]Invitation, len(rows))
		for i, r := range rows {
			out[i] = Invitation{ID: r.ID, Role: r.Role, Label: r.Label, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
				CreatedBy: r.CreatedByLogin, AcceptedBy: r.AcceptedByLogin, AcceptedAt: r.AcceptedAt, RevokedAt: r.RevokedAt,
				Status: invitationStatus(now, r.ExpiresAt, r.AcceptedAt, r.RevokedAt)}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("auth: list invitations: %w", err)
	}
	return out, nil
}

// RevokeInvitation stops an open invitation. Revoked and expired ones are left as they are; an
// accepted one is ErrInvitationAccepted.
func (s *Service) RevokeInvitation(ctx context.Context, a Actor, id uuid.UUID) error {
	now := s.clock.Now()
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		inv, err := q.LockInvitation(ctx, authdb.LockInvitationParams{TenantID: a.TenantID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvitationNotFound
		}
		if err != nil {
			return fmt.Errorf("lock invitation: %w", err)
		}
		switch invitationStatus(now, inv.ExpiresAt, inv.AcceptedAt, inv.RevokedAt) {
		case InvitationAccepted:
			return ErrInvitationAccepted
		case InvitationRevoked, InvitationExpired:
			return nil
		}
		if _, err := q.RevokeInvitation(ctx, authdb.RevokeInvitationParams{TenantID: a.TenantID, ID: id, Now: &now}); err != nil {
			return fmt.Errorf("revoke invitation: %w", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: a.TenantID, ActorUserID: &a.UserID, Action: ActionInvitationRevoked,
			EntityType: "invitation", EntityID: id.String(), IP: a.IP,
			Metadata: map[string]any{"role": inv.Role},
		})
	})
	if err != nil && !errors.Is(err, ErrInvitationNotFound) && !errors.Is(err, ErrInvitationAccepted) {
		return fmt.Errorf("auth: revoke invitation: %w", err)
	}
	return err
}

// invitationTarget is an open invitation found by its token before the tenant is known.
type invitationTarget struct {
	tenantID uuid.UUID
	preview  InvitationPreview
}

// lookupInvitation finds an open invitation through auth_invitation_lookup. A malformed or
// unknown token, and every closed invitation, is ErrInvitationInvalid.
func (s *Service) lookupInvitation(ctx context.Context, token string) (invitationTarget, error) {
	if !validTokenFormat(token) {
		return invitationTarget{}, ErrInvitationInvalid
	}
	var t invitationTarget
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		return tx.QueryRow(ctx, `SELECT invitation_id, tenant_id, tenant_name, role, expires_at FROM auth_invitation_lookup($1)`,
			hashToken(token)).Scan(&id, &t.tenantID, &t.preview.OrganizationName, &t.preview.Role, &t.preview.ExpiresAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return invitationTarget{}, ErrInvitationInvalid
	}
	if err != nil {
		return invitationTarget{}, fmt.Errorf("auth: look up invitation: %w", err)
	}
	return t, nil
}

// reserveInvite counts an invitation attempt of the client IP; the caller refunds it on success.
func (s *Service) reserveInvite(ctx context.Context, ip netip.Addr) (string, error) {
	key := limiterIPKey(ip)
	if ok, wait := s.inviteLimit.Reserve(key); !ok {
		s.log.WarnContext(ctx, "invitation attempts rate limited")
		return key, &RateLimitedError{RetryAfter: wait}
	}
	return key, nil
}

// LookupInvitation answers what an invitation link offers. Failures count against the per-IP
// invitation limit.
func (s *Service) LookupInvitation(ctx context.Context, token string, ip netip.Addr) (InvitationPreview, error) {
	key, err := s.reserveInvite(ctx, ip)
	if err != nil {
		return InvitationPreview{}, err
	}
	t, err := s.lookupInvitation(ctx, token)
	if err != nil {
		return InvitationPreview{}, err
	}
	s.inviteLimit.Refund(key)
	return t.preview, nil
}

// joinInvited adds userID to the invitation's organization inside tx (a transaction bound to
// that tenant): tenant row lock, the invitation locked and re-checked, the member quota, the
// membership, the invitation marked used and the audit row, all at once (ADR-0102).
func (s *Service) joinInvited(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, token string, ip netip.Addr) (string, error) {
	if err := plans.CheckJoin(ctx, tx, tenantID, plans.ResourceMember); err != nil {
		if errors.Is(err, tenants.ErrNotFound) {
			return "", ErrInvitationInvalid
		}
		return "", err
	}
	q := authdb.New(tx)
	inv, err := q.LockInvitationByHash(ctx, authdb.LockInvitationByHashParams{TenantID: tenantID, TokenHash: hashToken(token)})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvitationInvalid
	}
	if err != nil {
		return "", fmt.Errorf("lock invitation: %w", err)
	}
	now := s.clock.Now()
	if invitationStatus(now, inv.ExpiresAt, inv.AcceptedAt, inv.RevokedAt) != InvitationOpen {
		return "", ErrInvitationInvalid
	}
	if _, err := q.GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: tenantID, UserID: userID}); err == nil {
		return "", ErrAlreadyMember
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("get membership: %w", err)
	}
	if err := q.InsertMembership(ctx, authdb.InsertMembershipParams{TenantID: tenantID, UserID: userID, Role: inv.Role}); err != nil {
		return "", fmt.Errorf("insert membership: %w", err)
	}
	if n, err := q.MarkInvitationAccepted(ctx, authdb.MarkInvitationAcceptedParams{TenantID: tenantID, ID: inv.ID, Now: &now, UserID: &userID}); err != nil || n != 1 {
		return "", fmt.Errorf("mark invitation accepted (%d): %w", n, err)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorUserID: &userID, Action: ActionMemberJoined,
		EntityType: "user", EntityID: userID.String(), IP: ip,
		Metadata: map[string]any{"invitation_id": inv.ID.String(), "role": inv.Role},
	}); err != nil {
		return "", err
	}
	return inv.Role, nil
}

// AcceptInvitation makes the logged-in user a member of the inviting organization and switches
// the session there (a new session replaces p).
func (s *Service) AcceptInvitation(ctx context.Context, p Principal, token string, ip netip.Addr, userAgent string) (LoginResult, error) {
	key, err := s.reserveInvite(ctx, ip)
	if err != nil {
		return LoginResult{}, err
	}
	t, err := s.lookupInvitation(ctx, token)
	if err != nil {
		return LoginResult{}, err
	}
	var res LoginResult
	err = db.WithTenantTx(ctx, s.pool, t.tenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		u, err := q.GetUserByID(ctx, p.UserID)
		if err != nil || u.Status != userStatusActive {
			return ErrUnauthenticated
		}
		if u.IsPlatformAdmin {
			// BootstrapAdmin refuses to start when the admin belongs to another tenant (ADR-0095).
			return ErrAlreadyMember
		}
		role, err := s.joinInvited(ctx, tx, t.tenantID, p.UserID, token, ip)
		if err != nil {
			return err
		}
		tenant, err := tenants.Get(ctx, tx, t.tenantID)
		if err != nil {
			return err
		}
		sess, err := s.insertSession(ctx, q, t.tenantID, p.UserID, ip, userAgent)
		if err != nil {
			return err
		}
		res = LoginResult{Token: sess.token, SessionInfo: SessionInfo{
			User: User{ID: u.ID, Login: u.Email, DisplayName: u.DisplayName}, Tenant: tenant, Role: role,
			CSRFToken: sess.csrf, ExpiresAt: sess.expiresAt,
		}}
		return nil
	})
	if err != nil {
		var limit *plans.LimitError
		switch {
		case errors.Is(err, ErrInvitationInvalid):
			return LoginResult{}, err
		case errors.Is(err, ErrAlreadyMember), errors.Is(err, ErrUnauthenticated), errors.As(err, &limit):
			// A real invitation: the attempt does not count as guessing.
			s.inviteLimit.Refund(key)
			return LoginResult{}, err
		}
		s.inviteLimit.Refund(key)
		return LoginResult{}, fmt.Errorf("auth: accept invitation: %w", err)
	}
	s.inviteLimit.Refund(key)
	s.endSession(ctx, p)
	s.log.InfoContext(ctx, "invitation accepted", "user_id", p.UserID, "tenant_id", t.tenantID)
	return res, nil
}
