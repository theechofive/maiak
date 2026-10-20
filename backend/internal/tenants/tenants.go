// Package tenants owns the tenants table. Other modules read and create tenants only through
// these functions, inside the caller's transaction.
package tenants

import (
	"context"
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
