package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationPlans checks the plans migration (P-01, ADR-0093): the seeded plans, the
// default tenant moved to internal when the migration runs over existing data, new tenants on
// free, and plans read-only for the runtime roles.
func TestIntegrationPlans(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("plans"))
	ctx := context.Background()
	admin := env.AdminConn(t)

	var seeded string
	if err := admin.QueryRow(ctx, `SELECT string_agg(key || '=' || name || ' ' || limits::text, '; ' ORDER BY key) FROM plans`).Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	want := `free=Free {"max_channels": 1, "max_monitors": 3, "max_heartbeats": 1}; ` +
		`internal=Internal {"max_channels": 10, "max_monitors": 1000, "max_heartbeats": 100}`
	if seeded != want {
		t.Fatalf("plans = %s\nwant    %s", seeded, want)
	}

	// Replay the migration over a database of the previous release that already has tenants.
	pgtest.MustExec(t, admin,
		`ALTER TABLE tenants DROP COLUMN plan_key`,
		`DROP TABLE plans`,
		`DELETE FROM goose_db_version WHERE version_id = 20261021090000`,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'default', 'Default'), ('`+tenantB+`', 'b', 'B')`)
	if n, err := env.Migrate(ctx); err != nil || n != 1 {
		t.Fatalf("re-applied %d migrations, err=%v; want 1", n, err)
	}
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('01920000-0000-7000-8000-00000000000c', 'c', 'C')`)
	var plansBySlug string
	if err := admin.QueryRow(ctx, `SELECT string_agg(slug || '=' || plan_key, ',' ORDER BY slug) FROM tenants`).Scan(&plansBySlug); err != nil {
		t.Fatal(err)
	}
	if plansBySlug != "b=free,c=free,default=internal" {
		t.Fatalf("tenant plans = %s", plansBySlug)
	}
	if _, err := admin.Exec(ctx, `UPDATE tenants SET plan_key = 'gold' WHERE slug = 'b'`); err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("unknown plan key accepted: %v", err)
	}

	for _, url := range []string{env.APIURL, env.WorkerURL} {
		conn := env.Conn(t, url)
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM plans`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("runtime role reads plans: n=%d err=%v", n, err)
		}
		for _, stmt := range []string{
			`INSERT INTO plans (key, name, limits) VALUES ('gold', 'Gold', '{}')`,
			`UPDATE plans SET limits = '{"max_monitors": 100000}'`,
			`DELETE FROM plans WHERE key = 'free'`,
			`TRUNCATE plans`,
		} {
			if _, err := conn.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s: err = %v, want permission denied", stmt, err)
			}
		}
	}
}
