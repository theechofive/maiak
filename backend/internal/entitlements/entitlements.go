// Package entitlements answers which paid features an organization may use (ADR-0105). In the
// SaaS they come from the tenant's plan (plans.features); the offline-verified license key of
// self-hosted installations is a later source behind the same methods. Consumers declare the
// interface they need (for example the api's EntitlementsProvider) and get a *Service.
package entitlements

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/db"
)

// Service reads entitlements from the database.
type Service struct {
	pool *db.Pool
}

// NewService returns a Service.
func NewService(pool *db.Pool) *Service {
	return &Service{pool: pool}
}

// Features returns the features of a tenant, sorted; empty, never nil, when there are none.
func (s *Service) Features(ctx context.Context, tenantID uuid.UUID) ([]string, error) {
	var out []string
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = plans.Features(ctx, tx, tenantID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Has reports whether a tenant is entitled to feature.
func (s *Service) Has(ctx context.Context, tenantID uuid.UUID, feature string) (bool, error) {
	features, err := s.Features(ctx, tenantID)
	if err != nil {
		return false, err
	}
	return Includes(features, feature), nil
}

// Includes reports whether feature is in a sorted feature list.
func Includes(features []string, feature string) bool {
	if feature == "" {
		return false
	}
	_, ok := slices.BinarySearch(features, feature)
	return ok
}
