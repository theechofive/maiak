// Package tenants owns the tenants table. Other modules read and create tenants only through
// these functions, inside the caller's transaction.
package tenants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// Ensure returns the tenant with slug, creating it with name if it does not exist. Safe to run
// concurrently from several replicas.
func Ensure(ctx context.Context, tx pgx.Tx, slug, name string) (Tenant, error) {
	q := tenantsdb.New(tx)
	if err := q.InsertTenantIfMissing(ctx, tenantsdb.InsertTenantIfMissingParams{ID: uuidv7.New(), Slug: slug, Name: name}); err != nil {
		return Tenant{}, fmt.Errorf("tenants: insert %q: %w", slug, err)
	}
	row, err := q.GetTenantBySlug(ctx, slug)
	if err != nil {
		return Tenant{}, notFound(err, slug)
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

// DefaultMaxMonitors applies when tenants.limits has no max_monitors key (ADR-0016).
const DefaultMaxMonitors = 50

// Limits are the per-tenant quotas stored in tenants.limits.
type Limits struct {
	MaxMonitors int
}

// LockLimits locks the tenant row until the end of tx and returns its limits. Callers that
// check a quota and then insert hold this lock, so concurrent requests cannot both pass the
// check.
func LockLimits(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Limits, error) {
	raw, err := tenantsdb.New(tx).LockTenantLimits(ctx, id)
	if err != nil {
		return Limits{}, notFound(err, id.String())
	}
	var stored struct {
		MaxMonitors *int `json:"max_monitors"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Limits{}, fmt.Errorf("tenants: limits of %s: %w", id, err)
	}
	l := Limits{MaxMonitors: DefaultMaxMonitors}
	if stored.MaxMonitors != nil {
		l.MaxMonitors = *stored.MaxMonitors
	}
	return l, nil
}
