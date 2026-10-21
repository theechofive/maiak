// Package admin is the platform admin panel (P-04, ADR-0097): tenants across the platform (list,
// plan, suspension, soft delete), their users and password resets. It owns no table. Every read
// and write goes through the admin_* SECURITY DEFINER functions, which check inside that the
// acting user is a platform admin, enforce the guard rails and write the audit rows; the owning
// modules' tables are never touched directly from here.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
)

// Tenant statuses an admin may set.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
)

// Audit actions written by the admin functions (also the label values of
// maiak_admin_actions_total).
const (
	ActionPlanChanged   = "admin.tenant.plan_changed"
	ActionSuspended     = "admin.tenant.suspended"
	ActionUnsuspended   = "admin.tenant.unsuspended"
	ActionDeleted       = "admin.tenant.deleted"
	ActionPasswordReset = "admin.user.password_reset"
)

// Pagination of the list operations.
const (
	DefaultLimit = 25
	MaxLimit     = 100
)

const (
	// temporaryPasswordLen random characters from a 32-character alphabet: 120 bits.
	temporaryPasswordLen = 24
	// The alphabet leaves out 0, 1, l and o so a password read aloud or copied by hand survives.
	temporaryPasswordAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"
)

// Errors of the admin operations, mapped to HTTP problems in internal/api.
var (
	// ErrForbidden means the acting user is not an active platform admin.
	ErrForbidden = errors.New("admin: not a platform admin")
	// ErrNotFound means the tenant or user does not exist.
	ErrNotFound = errors.New("admin: not found")
	// ErrProtectedTenant means the default tenant cannot be suspended or deleted.
	ErrProtectedTenant = errors.New("admin: the default tenant cannot be suspended or deleted")
	// ErrOwnTenant means an admin cannot suspend or delete a tenant it belongs to.
	ErrOwnTenant = errors.New("admin: cannot suspend or delete a tenant you belong to")
	// ErrSystemAdmin means the password of a platform admin is managed by its Secret.
	ErrSystemAdmin = errors.New("admin: the system admin's password is managed by its Secret")
	// ErrTenantDeleted means the tenant is deleted and cannot be changed any more.
	ErrTenantDeleted = errors.New("admin: tenant is deleted")
)

// Options configure a Service.
type Options struct {
	Logger     *slog.Logger
	Registerer prometheus.Registerer
}

// Service implements the admin operations.
type Service struct {
	pool    *db.Pool
	auth    *auth.Service
	log     *slog.Logger
	actions *prometheus.CounterVec
}

// NewService builds the service. The auth service hashes temporary passwords through its bounded
// argon2 queue.
func NewService(pool *db.Pool, authSvc *auth.Service, opts Options) (*Service, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Service{pool: pool, auth: authSvc, log: opts.Logger,
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_admin_actions_total",
			Help: "Platform admin changes by audit action (admin.tenant.plan_changed, admin.tenant.suspended, admin.tenant.unsuspended, admin.tenant.deleted, admin.user.password_reset).",
		}, []string{"action"}),
	}
	if opts.Registerer != nil {
		if err := opts.Registerer.Register(s.actions); err != nil {
			return nil, fmt.Errorf("admin: register metrics: %w", err)
		}
	}
	return s, nil
}

// Actor is the platform admin performing an operation: the user of the session and the client
// address recorded in the audit rows.
type Actor struct {
	UserID uuid.UUID
	IP     netip.Addr
}

func (a Actor) ip() *netip.Addr {
	if a.IP.IsValid() {
		return &a.IP
	}
	return nil
}

// Tenant is a tenant as the admin sees it, with its usage.
type Tenant struct {
	ID         uuid.UUID
	Slug       string
	Name       string
	PlanKey    string
	PlanName   string
	Status     string
	CreatedAt  time.Time
	DeletedAt  *time.Time
	Users      int
	Monitors   int
	Heartbeats int
}

// Page selects a slice of a list.
type Page struct {
	Query  string
	Limit  int
	Offset int
}

// normalized applies the defaults and bounds of the contract.
func (p Page) normalized() Page {
	if p.Limit <= 0 {
		p.Limit = DefaultLimit
	}
	p.Limit = min(p.Limit, MaxLimit)
	p.Offset = max(p.Offset, 0)
	return p
}

// TenantList is one page of tenants and the number of all matches.
type TenantList struct {
	Items []Tenant
	Total int
}

// User is a member of a tenant as the admin sees it.
type User struct {
	ID              uuid.UUID
	Login           string
	Status          string
	Role            string
	CreatedAt       time.Time
	IsPlatformAdmin bool
}

// UserList is one page of users and the number of all matches.
type UserList struct {
	Items []User
	Total int
}

// PlanCount is a plan and its number of tenants that are not deleted.
type PlanCount struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Tenants int    `json:"tenants"`
}

// Stats are the platform-wide counts.
type Stats struct {
	Tenants struct {
		Total     int `json:"total"`
		Active    int `json:"active"`
		Suspended int `json:"suspended"`
		Deleted   int `json:"deleted"`
	} `json:"tenants"`
	Users      int         `json:"users"`
	Monitors   int         `json:"monitors"`
	Heartbeats int         `json:"heartbeats"`
	Plans      []PlanCount `json:"plans"`
}

// Stats returns the platform-wide counts.
func (s *Service) Stats(ctx context.Context, actor Actor) (Stats, error) {
	var raw []byte
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT admin_stats($1)`, actor.UserID).Scan(&raw)
	})
	if err != nil {
		return Stats{}, mapError("stats", err)
	}
	var st Stats
	if err := json.Unmarshal(raw, &st); err != nil {
		return Stats{}, fmt.Errorf("admin: stats: %w", err)
	}
	return st, nil
}

const tenantColumns = `id, slug, name, plan_key, plan_name, status, created_at, deleted_at, user_count, monitor_count, heartbeat_count`

func scanTenant(row pgx.Row, extra ...any) (Tenant, error) {
	var t Tenant
	dest := append([]any{&t.ID, &t.Slug, &t.Name, &t.PlanKey, &t.PlanName, &t.Status, &t.CreatedAt, &t.DeletedAt,
		&t.Users, &t.Monitors, &t.Heartbeats}, extra...)
	err := row.Scan(dest...)
	return t, err
}

// ListTenants returns a page of tenants, newest first. Query matches the slug, the name, the id or
// a member's login; deleted tenants are left out unless includeDeleted.
func (s *Service) ListTenants(ctx context.Context, actor Actor, page Page, includeDeleted bool) (TenantList, error) {
	page = page.normalized()
	list := TenantList{Items: []Tenant{}}
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		list.Items, list.Total = []Tenant{}, 0
		// Hand-written: sqlc cannot infer the columns of a RETURNS TABLE function.
		rows, err := tx.Query(ctx, `SELECT `+tenantColumns+`, total_count FROM admin_list_tenants($1, $2, $3, $4, $5)`,
			actor.UserID, page.Query, includeDeleted, page.Limit, page.Offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTenant(rows, &list.Total)
			if err != nil {
				return err
			}
			list.Items = append(list.Items, t)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(list.Items) == 0 && page.Offset > 0 {
			// A page past the end carries no row with the total; count from the start instead.
			return tx.QueryRow(ctx, `SELECT COALESCE(max(total_count), 0) FROM admin_list_tenants($1, $2, $3, 1, 0)`,
				actor.UserID, page.Query, includeDeleted).Scan(&list.Total)
		}
		return nil
	})
	if err != nil {
		return TenantList{}, mapError("list tenants", err)
	}
	return list, nil
}

// GetTenant returns one tenant, deleted ones included.
func (s *Service) GetTenant(ctx context.Context, actor Actor, id uuid.UUID) (Tenant, error) {
	var t Tenant
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		t, err = getTenant(ctx, tx, actor, id)
		return err
	})
	if err != nil {
		return Tenant{}, mapError("get tenant", err)
	}
	return t, nil
}

func getTenant(ctx context.Context, tx pgx.Tx, actor Actor, id uuid.UUID) (Tenant, error) {
	return scanTenant(tx.QueryRow(ctx, `SELECT `+tenantColumns+` FROM admin_get_tenant($1, $2)`, actor.UserID, id))
}

// TenantUpdate is a change of a tenant; nil fields stay as they are.
type TenantUpdate struct {
	PlanKey *string
	Status  *string
}

// UpdateTenant changes the plan and/or the status (active or suspended) of a live tenant in one
// transaction and returns the tenant afterwards. A plan change keeps every existing monitor and
// channel; usage above the new limits only blocks new creations (ADR-0093, ADR-0097).
func (s *Service) UpdateTenant(ctx context.Context, actor Actor, id uuid.UUID, upd TenantUpdate) (Tenant, error) {
	var verr validation.Error
	if upd.PlanKey == nil && upd.Status == nil {
		verr.Add("planKey", "give planKey or status")
	}
	if upd.PlanKey != nil && *upd.PlanKey == "" {
		verr.Add("planKey", "must not be empty")
	}
	if upd.Status != nil && *upd.Status != StatusActive && *upd.Status != StatusSuspended {
		verr.Add("status", "must be active or suspended")
	}
	if err := verr.Err(); err != nil {
		return Tenant{}, err
	}
	var t Tenant
	var done []string
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		done = done[:0]
		if upd.PlanKey != nil {
			changed, err := callChange(ctx, tx, `SELECT admin_set_tenant_plan($1, $2, $3, $4, $5)`,
				actor.UserID, id, *upd.PlanKey, uuidv7.New(), actor.ip())
			if err != nil {
				return err
			}
			if changed {
				done = append(done, ActionPlanChanged)
			}
		}
		if upd.Status != nil {
			changed, err := callChange(ctx, tx, `SELECT admin_set_tenant_status($1, $2, $3, $4, $5)`,
				actor.UserID, id, *upd.Status, uuidv7.New(), actor.ip())
			if err != nil {
				return err
			}
			if changed && *upd.Status == StatusSuspended {
				done = append(done, ActionSuspended)
			} else if changed {
				done = append(done, ActionUnsuspended)
			}
		}
		var err error
		t, err = getTenant(ctx, tx, actor, id)
		return err
	})
	if err != nil {
		return Tenant{}, mapError("update tenant", err)
	}
	for _, a := range done {
		s.recorded(ctx, a, "tenant_id", id)
	}
	return t, nil
}

// DeleteTenant soft-deletes a tenant (idempotent): its data stays, its users can no longer log
// in, its sessions are refused and nothing of it is checked or delivered any more.
func (s *Service) DeleteTenant(ctx context.Context, actor Actor, id uuid.UUID) error {
	var changed bool
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		changed, err = callChange(ctx, tx, `SELECT admin_delete_tenant($1, $2, $3, $4)`, actor.UserID, id, uuidv7.New(), actor.ip())
		return err
	})
	if err != nil {
		return mapError("delete tenant", err)
	}
	if changed {
		s.recorded(ctx, ActionDeleted, "tenant_id", id)
	}
	return nil
}

func callChange(ctx context.Context, tx pgx.Tx, sql string, args ...any) (bool, error) {
	var changed bool
	err := tx.QueryRow(ctx, sql, args...).Scan(&changed)
	return changed, err
}

// ListUsers returns a page of the members of a tenant (deleted tenants included), oldest
// membership first. Query matches the login.
func (s *Service) ListUsers(ctx context.Context, actor Actor, tenantID uuid.UUID, page Page) (UserList, error) {
	page = page.normalized()
	list := UserList{Items: []User{}}
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		list.Items, list.Total = []User{}, 0
		rows, err := tx.Query(ctx, `SELECT id, login, status, created_at, is_platform_admin, role, total_count
			FROM admin_list_users($1, $2, $3, $4, $5)`, actor.UserID, tenantID, page.Query, page.Limit, page.Offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u User
			if err := rows.Scan(&u.ID, &u.Login, &u.Status, &u.CreatedAt, &u.IsPlatformAdmin, &u.Role, &list.Total); err != nil {
				return err
			}
			list.Items = append(list.Items, u)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(list.Items) == 0 && page.Offset > 0 {
			return tx.QueryRow(ctx, `SELECT COALESCE(max(total_count), 0) FROM admin_list_users($1, $2, $3, 1, 0)`,
				actor.UserID, tenantID, page.Query).Scan(&list.Total)
		}
		return nil
	})
	if err != nil {
		return UserList{}, mapError("list users", err)
	}
	return list, nil
}

// ResetPassword gives the user a random temporary password and deletes all of its sessions. The
// password is returned once; only its argon2id hash reaches the database, and neither is logged
// nor audited. The configured system admin (any platform admin) is refused with ErrSystemAdmin.
func (s *Service) ResetPassword(ctx context.Context, actor Actor, userID uuid.UUID) (string, error) {
	// Checked before the expensive hash, so a non-admin cannot occupy the argon2 queue. The
	// definer function checks again.
	if ok, err := s.auth.IsPlatformAdmin(ctx, actor.UserID); err != nil {
		return "", err
	} else if !ok {
		return "", ErrForbidden
	}
	password, err := temporaryPassword()
	if err != nil {
		return "", err
	}
	hash, err := s.auth.HashPassword(ctx, password)
	if err != nil {
		return "", err
	}
	var tenantID uuid.UUID
	var revoked int
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id, sessions_revoked FROM admin_reset_user_password($1, $2, $3, $4, $5)`,
			actor.UserID, userID, hash, uuidv7.New(), actor.ip()).Scan(&tenantID, &revoked)
	})
	if err != nil {
		return "", mapError("reset password", err)
	}
	s.recorded(ctx, ActionPasswordReset, "target_user_id", userID, "target_tenant_id", tenantID, "sessions_revoked", revoked)
	return password, nil
}

// temporaryPassword returns temporaryPasswordLen characters of temporaryPasswordAlphabet. It
// satisfies the registration policy (12-128 characters).
func temporaryPassword() (string, error) {
	raw := make([]byte, temporaryPasswordLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("admin: random password: %w", err)
	}
	out := make([]byte, temporaryPasswordLen)
	for i, b := range raw {
		// 256 is a multiple of 32: no modulo bias.
		out[i] = temporaryPasswordAlphabet[int(b)%len(temporaryPasswordAlphabet)]
	}
	return string(out), nil
}

func (s *Service) recorded(ctx context.Context, action string, attrs ...any) {
	s.actions.WithLabelValues(action).Inc()
	s.log.InfoContext(ctx, "platform admin action", append([]any{"action", action}, attrs...)...)
}

// mapError turns the SQLSTATEs raised by the admin functions into the errors of this package.
func mapError(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "MKA01":
			return ErrForbidden
		case "MKA02":
			return ErrNotFound
		case "MKA03":
			return &validation.Error{Fields: []validation.FieldError{{Field: "planKey", Message: "unknown plan"}}}
		case "MKA04":
			return &validation.Error{Fields: []validation.FieldError{{Field: "status", Message: "must be active or suspended"}}}
		case "MKA05":
			return ErrProtectedTenant
		case "MKA06":
			return ErrOwnTenant
		case "MKA07":
			return ErrSystemAdmin
		case "MKA08":
			return ErrTenantDeleted
		}
	}
	return fmt.Errorf("admin: %s: %w", op, err)
}
