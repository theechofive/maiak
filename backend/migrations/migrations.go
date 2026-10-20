// Package migrations embeds the forward-only goose SQL migrations and applies them.
//
// Rules (enforced by migrations_test.go and the schema-lint integration test):
//   - file name YYYYMMDDHHMMSS_snake_case.sql (UTC timestamp); no "+goose Down" sections;
//   - each migration starts with SET LOCAL lock_timeout = '5s' (SET lock_timeout in a
//     "-- +goose NO TRANSACTION" migration);
//   - expand/contract: a migration must work with the previous app version;
//   - every table with tenant_id has RLS enabled and forced, and a policy.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// FS holds the SQL migration files.
//
//go:embed *.sql
var FS embed.FS

// Up applies all pending migrations. A Postgres session advisory lock serialises concurrent
// runs (for example a retried Job overlapping a slow one).
func Up(ctx context.Context, db *sql.DB) ([]*goose.MigrationResult, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("migrations: locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, FS,
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return nil, fmt.Errorf("migrations: provider: %w", err)
	}
	res, err := p.Up(ctx)
	if err != nil {
		return res, fmt.Errorf("migrations: up: %w", err)
	}
	return res, nil
}
