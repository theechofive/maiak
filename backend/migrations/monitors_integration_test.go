package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const (
	tenantA      = "01920000-0000-7000-8000-00000000000a"
	tenantB      = "01920000-0000-7000-8000-00000000000b"
	monitorA     = "01920000-0000-7000-8000-0000000000a1"
	homeLocation = "01920000-0000-7000-8000-000000000001"
)

// TestIntegrationMonitorsSchema checks the grants, RLS and constraints of the monitors tables
// from the point of view of the runtime roles.
func TestIntegrationMonitorsSchema(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("monitorsschema"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	api := env.Conn(t, env.APIURL)
	worker := env.Conn(t, env.WorkerURL)

	// inTenant runs stmt in a transaction with app.tenant_id set and rolls back unless commit.
	inTenant := func(conn *pgx.Conn, tenant, stmt string, commit bool) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
		if commit {
			return tx.Commit(ctx)
		}
		return nil
	}
	mustFail := func(err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}

	// Locations are global and read-only for runtime roles.
	for name, conn := range map[string]*pgx.Conn{"api": api, "worker": worker} {
		var code string
		if err := conn.QueryRow(ctx, `SELECT code FROM probe_locations WHERE id = $1`, homeLocation).Scan(&code); err != nil || code != "home-eu-central-1" {
			t.Fatalf("%s reads home location: %q %v", name, code, err)
		}
		_, err := conn.Exec(ctx, `UPDATE probe_locations SET status = 'disabled'`)
		mustFail(err, "permission denied")
	}

	create := `
INSERT INTO monitors (id, tenant_id, name, config) VALUES ('` + monitorA + `', '` + tenantA + `', 'm', '{"url":"https://example.com"}');
INSERT INTO monitor_locations (tenant_id, monitor_id, location_id) VALUES ('` + tenantA + `', '` + monitorA + `', '` + homeLocation + `');
INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at) VALUES ('` + tenantA + `', '` + monitorA + `', '` + homeLocation + `', now());
INSERT INTO monitor_state (tenant_id, monitor_id) VALUES ('` + tenantA + `', '` + monitorA + `');`

	mustFail(inTenant(worker, tenantA, create, false), "permission denied")
	// A row for another tenant violates the policy's WITH CHECK.
	mustFail(inTenant(api, tenantB, create, false), "row-level security")
	if err := inTenant(api, tenantA, create, true); err != nil {
		t.Fatalf("api creates a monitor: %v", err)
	}

	// The other tenant and a connection without tenant see nothing.
	for _, tenant := range []string{tenantB, ""} {
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
			t.Fatal(err)
		}
		var n int
		err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM monitors) + (SELECT count(*) FROM monitor_locations)
			+ (SELECT count(*) FROM monitor_location_state) + (SELECT count(*) FROM monitor_state)`).Scan(&n)
		_ = tx.Rollback(ctx)
		if err != nil || n != 0 {
			t.Fatalf("tenant %q sees %d rows (err %v)", tenant, n, err)
		}
	}

	// Soft delete only; the worker updates state but never monitors.
	mustFail(inTenant(api, tenantA, `DELETE FROM monitors`, false), "permission denied")
	mustFail(inTenant(worker, tenantA, `UPDATE monitors SET paused = true`, false), "permission denied")
	if err := inTenant(worker, tenantA, `UPDATE monitor_location_state SET consecutive_failures = 1;
		UPDATE monitor_state SET status = 'down', status_changed_at = now()`, false); err != nil {
		t.Fatalf("worker updates state: %v", err)
	}
	if err := inTenant(api, tenantA, `UPDATE monitors SET deleted_at = now()`, false); err != nil {
		t.Fatalf("api soft-deletes: %v", err)
	}

	// MVP limits and value checks.
	for stmt, want := range map[string]string{
		`UPDATE monitors SET interval_seconds = 30`:    "monitors_mvp_interval",
		`UPDATE monitors SET timeout_seconds = 5`:      "monitors_mvp_timeout",
		`UPDATE monitors SET failure_threshold = 4`:    "check constraint",
		`UPDATE monitors SET type = 'tcp'`:             "check constraint",
		`UPDATE monitor_state SET status = 'degraded'`: "check constraint",
	} {
		mustFail(inTenant(api, tenantA, stmt, false), want)
	}
	// The schedule belongs to the worker (the api resets it only through reset_monitor_runtime).
	mustFail(inTenant(worker, tenantA, `UPDATE monitor_location_state SET lease_token = gen_random_uuid()`, false), "check constraint")
}
