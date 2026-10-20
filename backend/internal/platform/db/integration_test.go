package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

type fixture struct {
	tenantA, tenantB, user uuid.UUID
}

// seed creates two tenants with one member each, plus one session and one audit row per tenant,
// all through the api role.
func seed(t *testing.T, ctx context.Context, pool *db.Pool) fixture {
	t.Helper()
	f := fixture{tenantA: uuidv7.New(), tenantB: uuidv7.New(), user: uuidv7.New()}
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, 'tenant-a', 'A'), ($2, 'tenant-b', 'B')`, f.tenantA, f.tenantB); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, 'person@example.com')`, f.user)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, tenant := range []uuid.UUID{f.tenantA, f.tenantB} {
		err := db.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`, tenant, f.user); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, user_id, tenant_id, token_hash, csrf_token, expires_at)
				VALUES ($1, $2, $3, $4, 'csrf', now() + interval '1 hour')`, uuidv7.New(), f.user, tenant, hash32(byte(i))); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO audit_log (id, tenant_id, action, entity_type) VALUES ($1, $2, 'test', 'test')`, uuidv7.New(), tenant)
			return err
		})
		if err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
	}
	return f
}

func hash32(b byte) []byte {
	h := make([]byte, 32)
	for i := range h {
		h[i] = b
	}
	return h
}

func count(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestIntegrationRLSIsolation(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("db"))
	ctx := context.Background()
	api := env.Pool(t, env.APIURL, 4)
	f := seed(t, ctx, api)

	t.Run("tenant context sees only its rows", func(t *testing.T) {
		err := db.WithTenantTx(ctx, api, f.tenantA, func(tx pgx.Tx) error {
			for _, table := range []string{"memberships", "sessions"} {
				if n := count(t, ctx, tx, "SELECT count(*) FROM "+table); n != 1 {
					t.Errorf("%s: tenant A sees %d rows, want 1", table, n)
				}
				if n := count(t, ctx, tx, "SELECT count(*) FROM "+table+" WHERE tenant_id = $1", f.tenantB); n != 0 {
					t.Errorf("%s: tenant A sees %d rows of tenant B", table, n)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no tenant context sees nothing", func(t *testing.T) {
		err := db.WithTx(ctx, api, func(tx pgx.Tx) error {
			for _, table := range []string{"memberships", "sessions"} {
				if n := count(t, ctx, tx, "SELECT count(*) FROM "+table); n != 0 {
					t.Errorf("%s: %d rows visible without tenant context", table, n)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cannot write into another tenant", func(t *testing.T) {
		err := db.WithTenantTx(ctx, api, f.tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO audit_log (id, tenant_id, action, entity_type) VALUES ($1, $2, 'x', 'x')`, uuidv7.New(), f.tenantB)
			return err
		})
		if !isPgCode(err, "42501") || !strings.Contains(err.Error(), "row-level security") {
			t.Fatalf("cross-tenant insert: err = %v, want RLS violation", err)
		}
		var updated int64
		err = db.WithTenantTx(ctx, api, f.tenantA, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE memberships SET role = 'viewer' WHERE tenant_id = $1`, f.tenantB)
			updated = tag.RowsAffected()
			return err
		})
		if err != nil || updated != 0 {
			t.Fatalf("cross-tenant update touched %d rows, err=%v", updated, err)
		}
		err = db.WithTenantTx(ctx, api, f.tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE memberships SET tenant_id = $1 WHERE tenant_id = $2`, f.tenantB, f.tenantA)
			return err
		})
		if !isPgCode(err, "42501") {
			t.Fatalf("moving a row to another tenant: err = %v, want RLS violation", err)
		}
	})

	t.Run("session lookup works before tenant context and only for api", func(t *testing.T) {
		err := db.WithTx(ctx, api, func(tx pgx.Tx) error {
			var tenant uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT tenant_id FROM auth_lookup_session($1)`, hash32(1)).Scan(&tenant); err != nil {
				return err
			}
			if tenant != f.tenantB {
				t.Errorf("lookup returned tenant %s, want %s", tenant, f.tenantB)
			}
			if n := count(t, ctx, tx, `SELECT count(*) FROM auth_lookup_session($1)`, hash32(9)); n != 0 {
				t.Errorf("unknown hash returned %d rows", n)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		worker := env.Pool(t, env.WorkerURL, 1)
		err = db.WithTx(ctx, worker, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT * FROM auth_lookup_session($1)`, hash32(1))
			return err
		})
		if !isPgCode(err, "42501") {
			t.Fatalf("worker executing auth_lookup_session: err = %v, want permission denied", err)
		}
	})

	t.Run("grants", func(t *testing.T) {
		denied := map[string]string{
			"api reads audit_log":     `SELECT count(*) FROM audit_log`,
			"api deletes audit_log":   `DELETE FROM audit_log`,
			"api deletes users":       `DELETE FROM users`,
			"api deletes tenants":     `DELETE FROM tenants`,
			"api changes user id":     `UPDATE users SET id = id`,
			"api creates tables":      `CREATE TABLE evil (id int)`,
			"api creates functions":   `CREATE FUNCTION evil() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
			"api alters tenant table": `ALTER TABLE sessions DISABLE ROW LEVEL SECURITY`,
		}
		for name, stmt := range denied {
			err := db.WithTenantTx(ctx, api, f.tenantA, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			if !isPgCode(err, "42501") {
				t.Errorf("%s: err = %v, want permission denied", name, err)
			}
		}
		worker := env.Pool(t, env.WorkerURL, 1)
		for name, stmt := range map[string]string{
			"worker reads users":    `SELECT count(*) FROM users`,
			"worker reads sessions": `SELECT count(*) FROM sessions`,
		} {
			err := db.WithTenantTx(ctx, worker, f.tenantA, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			if !isPgCode(err, "42501") {
				t.Errorf("%s: err = %v, want permission denied", name, err)
			}
		}
	})
}

func TestIntegrationWithTenantTx(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("dbtx"))
	ctx := context.Background()
	// One connection, so every transaction below reuses the same session.
	pool := env.Pool(t, env.APIURL, 1)
	tenant := uuidv7.New()

	if err := db.WithTenantTx(ctx, pool, uuid.Nil, func(pgx.Tx) error { return nil }); !errors.Is(err, db.ErrNoTenant) {
		t.Fatalf("nil tenant: err = %v", err)
	}

	err := db.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
		var got string
		if err := tx.QueryRow(ctx, `SELECT current_setting('app.tenant_id')`).Scan(&got); err != nil {
			return err
		}
		if got != tenant.String() {
			t.Errorf("app.tenant_id = %q, want %q", got, tenant)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The setting is transaction-local and must not leak to the next user of the connection.
	err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var got *string
		if err := tx.QueryRow(ctx, `SELECT NULLIF(current_setting('app.tenant_id', true), '')`).Scan(&got); err != nil {
			return err
		}
		if got != nil {
			t.Errorf("app.tenant_id leaked into the next transaction: %q", *got)
		}
		// And a tenant table read on this connection returns nothing instead of failing.
		return tx.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(new(int))
	})
	if err != nil {
		t.Fatal(err)
	}

	// An error rolls the transaction back.
	boom := errors.New("boom")
	err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, 'rolled-back', 'x')`, uuidv7.New()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if n := count(t, ctx, tx, `SELECT count(*) FROM tenants WHERE slug = 'rolled-back'`); n != 0 {
			t.Error("insert was not rolled back")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
