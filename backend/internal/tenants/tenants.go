// Package tenants owns the tenants table. Other modules read and create tenants only through
// these functions, inside the caller's transaction.
package tenants

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/tenants/tenantsdb"
)

// DefaultSlug is the tenant the single admin is bound to in the MVP.
const DefaultSlug = "default"

// Tenant is a tenant as seen by other modules.
type Tenant struct {
	ID     uuid.UUID
	Slug   string
	Name   string
	Status string
}

// StatusActive is the status of a usable tenant.
const StatusActive = "active"

// ErrNotFound means no live tenant matches.
var ErrNotFound = errors.New("tenants: not found")

// Ensure returns the tenant with slug, creating it with name on planKey if it does not exist.
// An existing tenant keeps its plan. Safe to run concurrently from several replicas.
func Ensure(ctx context.Context, tx pgx.Tx, slug, name, planKey string) (Tenant, error) {
	q := tenantsdb.New(tx)
	if err := q.InsertTenantIfMissing(ctx, tenantsdb.InsertTenantIfMissingParams{ID: uuidv7.New(), Slug: slug, Name: name, PlanKey: planKey}); err != nil {
		return Tenant{}, fmt.Errorf("tenants: insert %q: %w", slug, err)
	}
	row, err := q.GetTenantBySlug(ctx, slug)
	if err != nil {
		return Tenant{}, notFound(err, slug)
	}
	return Tenant(row), nil
}

// ErrSlugTaken means another tenant already has the slug; the transaction is aborted.
var ErrSlugTaken = errors.New("tenants: slug taken")

// Create inserts a new tenant with id on planKey (registration, P-02).
func Create(ctx context.Context, tx pgx.Tx, id uuid.UUID, slug, name, planKey string) (Tenant, error) {
	row, err := tenantsdb.New(tx).InsertTenant(ctx, tenantsdb.InsertTenantParams{ID: id, Slug: slug, Name: name, PlanKey: planKey})
	if err != nil {
		if db.IsUniqueViolation(err, "tenants_slug_key") {
			return Tenant{}, ErrSlugTaken
		}
		return Tenant{}, fmt.Errorf("tenants: create %q: %w", slug, err)
	}
	return Tenant(row), nil
}

// Get returns a live tenant by id.
func Get(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Tenant, error) {
	row, err := tenantsdb.New(tx).GetTenantByID(ctx, id)
	if err != nil {
		return Tenant{}, notFound(err, id.String())
	}
	return Tenant(row), nil
}

func notFound(err error, key string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return fmt.Errorf("tenants: get %s: %w", key, err)
}

// Quota is the plan of a tenant and its per-tenant limit overrides (tenants.limits, raw jsonb);
// internal/plans resolves them into effective limits.
type Quota struct {
	PlanKey   string
	Overrides []byte
}

// LockQuota locks the tenant row until the end of tx and returns its quota. Callers that check
// a quota and then insert hold this lock, so concurrent requests cannot both pass the check.
func LockQuota(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Quota, error) {
	row, err := tenantsdb.New(tx).LockTenantQuota(ctx, id)
	if err != nil {
		return Quota{}, notFound(err, id.String())
	}
	return Quota{PlanKey: row.PlanKey, Overrides: row.Limits}, nil
}

// GetQuota returns the quota of a tenant without locking it.
func GetQuota(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Quota, error) {
	row, err := tenantsdb.New(tx).GetTenantQuota(ctx, id)
	if err != nil {
		return Quota{}, notFound(err, id.String())
	}
	return Quota{PlanKey: row.PlanKey, Overrides: row.Limits}, nil
}
