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
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/theechofive/maiak/backend/internal/platform/db"
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

// Latest is the newest migration version embedded in this binary.
func Latest() (int64, error) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		return 0, fmt.Errorf("migrations: list: %w", err)
	}
	var latest int64
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migrations: version of %s: %w", e.Name(), err)
		}
		latest = max(latest, v)
	}
	if latest == 0 {
		return 0, errors.New("migrations: no migrations embedded")
	}
	return latest, nil
}

// ErrSchemaAhead means the database has a migration this binary does not know: the image is
// older than the schema (an image rollback across a migration), and old code may misread or
// damage the newer schema (L-282, L-311).
var ErrSchemaAhead = errors.New("migrations: database schema is newer than this binary")

// CheckNotAhead refuses to run an application process against a schema that is newer than its
// newest embedded migration. api, worker and jobs call it at start; cmd/migrate does not (it
// is what moves the schema forward). A schema that is behind is fine: the migrate Job runs first.
func CheckNotAhead(ctx context.Context, p *db.Pool) error {
	latest, err := Latest()
	if err != nil {
		return err
	}
	var applied int64
	err = db.WithTx(ctx, p, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&applied)
	})
	if err != nil {
		return fmt.Errorf("migrations: read schema version: %w", err)
	}
	if applied > latest {
		return fmt.Errorf("%w: database at migration %d, binary knows up to %d; deploy a release that contains migration %d (rolling an image back across a migration is not supported, L-311)",
			ErrSchemaAhead, applied, latest, applied)
	}
	return nil
}
