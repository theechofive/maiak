// Package db owns the connection pool and is the only way application code reaches Postgres.
// The pool type is opaque: callers get a transaction from WithTx or WithTenantTx, never the
// pool itself, so tenant data cannot be queried without app.tenant_id being set (the depguard
// rule in .golangci.yml forbids importing pgxpool elsewhere).
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// Pool is a Postgres connection pool for a runtime role.
type Pool struct {
	p *pgxpool.Pool
}

// Options tune the pool.
type Options struct {
	// MaxConns caps connections; the role's CONNECTION LIMIT is shared by all replicas.
	MaxConns int32
	// ApplicationName shows up in pg_stat_activity.
	ApplicationName string
}

// Open creates a pool. It does not wait for the database to be reachable.
func Open(ctx context.Context, url string, opts Options) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The parse error can contain parts of the URL; do not wrap it.
		return nil, errors.New("db: invalid connection string")
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	if opts.ApplicationName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = opts.ApplicationName
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	return &Pool{p: p}, nil
}

// Ping checks that a connection can be acquired and the server answers.
func (p *Pool) Ping(ctx context.Context) error { return p.p.Ping(ctx) }

// Close closes all connections.
func (p *Pool) Close() { p.p.Close() }

// TxFunc is the body of a transaction. Returning an error rolls the transaction back.
type TxFunc func(tx pgx.Tx) error

// WithTx runs fn in a transaction without tenant context. Use it only for tables that are not
// tenant-scoped (users, tenants) and for SECURITY DEFINER lookups; tenant tables return no
// rows here because of RLS.
func WithTx(ctx context.Context, p *Pool, fn TxFunc) error {
	return pgx.BeginTxFunc(ctx, p.p, pgx.TxOptions{}, fn)
}

// ErrNoTenant is returned when WithTenantTx is called with the zero UUID.
var ErrNoTenant = errors.New("db: tenant id is required")

// WithTenantTx runs fn in a transaction with app.tenant_id set (transaction-local), so the
// row-level security policies restrict every statement to that tenant.
func WithTenantTx(ctx context.Context, p *Pool, tenantID uuid.UUID, fn TxFunc) error {
	if tenantID == uuid.Nil {
		return ErrNoTenant
	}
	logging.SetTenantID(ctx, tenantID.String())
	return pgx.BeginTxFunc(ctx, p.p, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := SetTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// SetTenant sets app.tenant_id for the rest of the transaction. WithTenantTx calls it; call it
// directly only when the tenant becomes known in the middle of a transaction (bootstrap).
func SetTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return ErrNoTenant
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("db: set tenant: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a unique violation (SQLSTATE 23505) of constraint.
// The transaction that produced it is aborted and must be rolled back.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
