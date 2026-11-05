package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/theechofive/maiak/backend/internal/audit"
	"github.com/theechofive/maiak/backend/internal/auth/authdb"
	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/tenants"
)

// Audit actions of member management (ADR-0102). Metadata never carries a token.
const (
	ActionMemberInvited        = "member.invited"
	ActionInvitationRevoked    = "invitation.revoked"
	ActionMemberJoined         = "member.joined"
	ActionMemberRoleChanged    = "member.role_changed"
	ActionMemberRemoved        = "member.removed"
	ActionOrganizationSwitched = "organization.switched"
)

var (
	// ErrMemberNotFound means the user is not a member of the organization.
	ErrMemberNotFound = errors.New("auth: member not found")
	// ErrLastOwner means the change would leave the organization without an active owner.
	ErrLastOwner = errors.New("auth: the organization must keep an active owner")
	// ErrSystemAdminMember means the change targets the platform admin, whose owner membership of
	// the default tenant comes from configuration (ADR-0097).
	ErrSystemAdminMember = errors.New("auth: the platform admin's membership cannot be changed")
	// ErrProtectedOrganization means the default organization cannot be deleted.
	ErrProtectedOrganization = errors.New("auth: the default organization cannot be deleted")
)

// Actor is the member acting in its session's organization. Its role is re-read inside every
// transaction that depends on it.
type Actor struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	IP       netip.Addr
}

// Member is one membership of an organization.
type Member struct {
	UserID          uuid.UUID
	Login           string
	DisplayName     string
	Role            string
	JoinedAt        time.Time
	IsPlatformAdmin bool
}

// ListMembers returns the members of the organization, oldest first.
func (s *Service) ListMembers(ctx context.Context, tenantID uuid.UUID) ([]Member, error) {
	var out []Member
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := authdb.New(tx).ListMembers(ctx, tenantID)
		if err != nil {
			return err
		}
		out = make([]Member, len(rows))
		for i, r := range rows {
			out[i] = Member{UserID: r.UserID, Login: r.Email, DisplayName: r.DisplayName, Role: r.Role,
				JoinedAt: r.CreatedAt, IsPlatformAdmin: r.IsPlatformAdmin}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("auth: list members: %w", err)
	}
	return out, nil
}

// lockActorRole takes the tenant row lock, which serialises every role change, removal and
// acceptance of one organization (so two owners cannot demote each other at once), and returns
// the actor's current role.
func lockActorRole(ctx context.Context, tx pgx.Tx, a Actor) (authz.Actor, error) {
	if _, err := tenants.LockQuota(ctx, tx, a.TenantID); err != nil {
		return authz.Actor{}, err
	}
	role, err := authdb.New(tx).GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: a.TenantID, UserID: a.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return authz.Actor{}, ErrUnauthenticated
	}
	if err != nil {
		return authz.Actor{}, fmt.Errorf("get actor role: %w", err)
	}
	return authz.Actor{UserID: a.UserID, Role: authz.Role(role)}, nil
}

// lockTarget locks the target membership and applies the rules that need the database: the
// platform admin is never changed and the last active owner never loses the role.
func lockTarget(ctx context.Context, q *authdb.Queries, tenantID, userID uuid.UUID, losesOwner bool) (authdb.LockMemberRow, error) {
	m, err := q.LockMember(ctx, authdb.LockMemberParams{TenantID: tenantID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrMemberNotFound
	}
	if err != nil {
		return m, fmt.Errorf("lock member: %w", err)
	}
	if m.IsPlatformAdmin {
		return m, ErrSystemAdminMember
	}
	if m.Role == RoleOwner && losesOwner {
		n, err := q.CountOtherActiveOwners(ctx, authdb.CountOtherActiveOwnersParams{TenantID: tenantID, UserID: userID})
		if err != nil {
			return m, fmt.Errorf("count owners: %w", err)
		}
		if n == 0 {
			return m, ErrLastOwner
		}
	}
	return m, nil
}

// ChangeRole gives a member another role (admin or owner only; an admin never touches an owner
// and nobody grants a role above their own, authz.CanManage).
func (s *Service) ChangeRole(ctx context.Context, a Actor, userID uuid.UUID, role string) (Member, error) {
	if !authz.Valid(authz.Role(role)) {
		return Member{}, fmt.Errorf("%w: unknown role", authz.ErrForbidden)
	}
	var out Member
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		actor, err := lockActorRole(ctx, tx, a)
		if err != nil {
			return err
		}
		q := authdb.New(tx)
		m, err := q.LockMember(ctx, authdb.LockMemberParams{TenantID: a.TenantID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMemberNotFound
		}
		if err != nil {
			return fmt.Errorf("lock member: %w", err)
		}
		if !authz.CanManage(actor, authz.Role(m.Role), authz.Role(role)) {
			return authz.ErrForbidden
		}
		out = Member{UserID: userID, Login: m.Email, DisplayName: m.DisplayName, Role: m.Role, JoinedAt: m.CreatedAt, IsPlatformAdmin: m.IsPlatformAdmin}
		if m.Role == role {
			return nil
		}
		if _, err := lockTarget(ctx, q, a.TenantID, userID, role != RoleOwner); err != nil {
			return err
		}
		if _, err := q.UpdateMembershipRole(ctx, authdb.UpdateMembershipRoleParams{TenantID: a.TenantID, UserID: userID, Role: role}); err != nil {
			return fmt.Errorf("update role: %w", err)
		}
		out.Role = role
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: a.TenantID, ActorUserID: &a.UserID, Action: ActionMemberRoleChanged,
			EntityType: "user", EntityID: userID.String(), IP: a.IP,
			Metadata: map[string]any{"from": m.Role, "to": role},
		})
	})
	if err != nil {
		return Member{}, wrapMemberErr("change role", err)
	}
	return out, nil
}

// RemoveMember ends a membership and with it the user's sessions in the organization; the user
// and its other memberships stay. The rules of ChangeRole apply.
func (s *Service) RemoveMember(ctx context.Context, a Actor, userID uuid.UUID) error {
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		actor, err := lockActorRole(ctx, tx, a)
		if err != nil {
			return err
		}
		q := authdb.New(tx)
		m, err := q.LockMember(ctx, authdb.LockMemberParams{TenantID: a.TenantID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMemberNotFound
		}
		if err != nil {
			return fmt.Errorf("lock member: %w", err)
		}
		if !authz.CanManage(actor, authz.Role(m.Role), "") {
			return authz.ErrForbidden
		}
		return s.deleteMembership(ctx, tx, a, userID, false)
	})
	return wrapMemberErr("remove member", err)
}

// Leave ends the actor's own membership (any role); the last owner and the platform admin
// cannot leave.
func (s *Service) Leave(ctx context.Context, a Actor) error {
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		if _, err := lockActorRole(ctx, tx, a); err != nil {
			return err
		}
		return s.deleteMembership(ctx, tx, a, a.UserID, true)
	})
	return wrapMemberErr("leave", err)
}

func (s *Service) deleteMembership(ctx context.Context, tx pgx.Tx, a Actor, userID uuid.UUID, self bool) error {
	q := authdb.New(tx)
	m, err := lockTarget(ctx, q, a.TenantID, userID, true)
	if err != nil {
		return err
	}
	if _, err := q.DeleteMembership(ctx, authdb.DeleteMembershipParams{TenantID: a.TenantID, UserID: userID}); err != nil {
		return fmt.Errorf("delete membership: %w", err)
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: a.TenantID, ActorUserID: &a.UserID, Action: ActionMemberRemoved,
		EntityType: "user", EntityID: userID.String(), IP: a.IP,
		Metadata: map[string]any{"role": m.Role, "self": self},
	})
}

func wrapMemberErr(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrMemberNotFound), errors.Is(err, ErrLastOwner), errors.Is(err, ErrSystemAdminMember),
		errors.Is(err, authz.ErrForbidden), errors.Is(err, ErrUnauthenticated):
		return err
	}
	return fmt.Errorf("auth: %s: %w", op, err)
}

// DeleteOrganization soft-deletes the actor's organization (owner only, never the default one)
// through org_delete_tenant, which checks the owner again and writes the audit row.
func (s *Service) DeleteOrganization(ctx context.Context, a Actor) error {
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		var ip *netip.Addr
		if a.IP.IsValid() {
			ip = &a.IP
		}
		var deleted bool
		return tx.QueryRow(ctx, `SELECT org_delete_tenant($1, $2, $3)`, a.UserID, uuidv7.New(), ip).Scan(&deleted)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "MKO01":
			return ErrProtectedOrganization
		case "MKO02":
			return authz.ErrForbidden
		}
	}
	if err != nil {
		return fmt.Errorf("auth: delete organization: %w", err)
	}
	s.log.InfoContext(ctx, "organization deleted by its owner", "user_id", a.UserID)
	return nil
}

// Organization is one of the user's memberships, for the switcher.
type Organization struct {
	Tenant tenants.Tenant
	Role   string
}

// Organizations lists the user's organizations that are not deleted (auth_user_organizations).
func (s *Service) Organizations(ctx context.Context, userID uuid.UUID) ([]Organization, error) {
	var out []Organization
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, slug, name, status, role FROM auth_user_organizations($1)`, userID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Organization, error) {
			var o Organization
			err := r.Scan(&o.Tenant.ID, &o.Tenant.Slug, &o.Tenant.Name, &o.Tenant.Status, &o.Role)
			return o, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("auth: organizations: %w", err)
	}
	return out, nil
}

// ErrOrganizationNotFound means the user is not a member of a live organization with that id.
var ErrOrganizationNotFound = errors.New("auth: organization not found")

// SwitchOrganization replaces the session p with a new one bound to tenantID after checking the
// membership and that the organization is active (ADR-0102). The old session is deleted.
func (s *Service) SwitchOrganization(ctx context.Context, p Principal, tenantID uuid.UUID, ip netip.Addr, userAgent string) (LoginResult, error) {
	var res LoginResult
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := authdb.New(tx)
		role, err := q.GetMembershipRole(ctx, authdb.GetMembershipRoleParams{TenantID: tenantID, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrganizationNotFound
		}
		if err != nil {
			return fmt.Errorf("get membership: %w", err)
		}
		t, err := tenants.Get(ctx, tx, tenantID)
		if errors.Is(err, tenants.ErrNotFound) {
			return ErrOrganizationNotFound
		}
		if err != nil {
			return err
		}
		if t.Status != tenants.StatusActive {
			return ErrAccountSuspended
		}
		u, err := q.GetUserByID(ctx, p.UserID)
		if err != nil || u.Status != userStatusActive {
			return ErrUnauthenticated
		}
		sess, err := s.insertSession(ctx, q, tenantID, p.UserID, ip, userAgent)
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorUserID: &p.UserID, Action: ActionOrganizationSwitched,
			EntityType: "session", EntityID: sess.id.String(), IP: ip,
			Metadata: map[string]any{"from_tenant_id": p.TenantID.String()},
		}); err != nil {
			return err
		}
		res = LoginResult{Token: sess.token, SessionInfo: SessionInfo{
			User:   User{ID: u.ID, Login: u.Email, DisplayName: u.DisplayName, IsPlatformAdmin: u.IsPlatformAdmin},
			Tenant: t, Role: role, CSRFToken: sess.csrf, ExpiresAt: sess.expiresAt,
		}}
		return nil
	})
	switch {
	case errors.Is(err, ErrOrganizationNotFound), errors.Is(err, ErrAccountSuspended), errors.Is(err, ErrUnauthenticated):
		return LoginResult{}, err
	case err != nil:
		return LoginResult{}, fmt.Errorf("auth: switch organization: %w", err)
	}
	s.endSession(ctx, p)
	return res, nil
}

// endSession deletes the session p after it was replaced. A failure leaves the old session valid
// until it expires; it is logged, not returned, because the new session already exists.
func (s *Service) endSession(ctx context.Context, p Principal) {
	err := db.WithTenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		_, err := authdb.New(tx).DeleteSession(ctx, p.SessionID)
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "could not delete the replaced session", "session_id", p.SessionID, "error", err)
	}
}
