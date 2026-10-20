package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationAPIRuntimeGrants checks ADR-0018: maiak_api has no UPDATE on the schedule and
// only column grants on monitor_state; it resets a schedule through reset_monitor_runtime,
// which stays inside the caller's tenant.
func TestIntegrationAPIRuntimeGrants(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("runtimegrants"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	const monitorB = "01920000-0000-7000-8000-0000000000b1"
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	for _, m := range [][2]string{{tenantA, monitorA}, {tenantB, monitorB}} {
		pgtest.MustExec(t, admin,
			`INSERT INTO monitors (id, tenant_id, name, config) VALUES ('`+m[1]+`', '`+m[0]+`', 'm', '{"url":"https://example.com"}')`,
			`INSERT INTO monitor_locations (tenant_id, monitor_id, location_id) VALUES ('`+m[0]+`', '`+m[1]+`', '`+homeLocation+`')`,
			`INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at, status, consecutive_failures, lease_token, lease_until)
			 VALUES ('`+m[0]+`', '`+m[1]+`', '`+homeLocation+`', now() + interval '1 hour', 'down', 3, gen_random_uuid(), now() + interval '30 seconds')`,
			`INSERT INTO monitor_state (tenant_id, monitor_id) VALUES ('`+m[0]+`', '`+m[1]+`')`)
	}
	api := env.Conn(t, env.APIURL)
	worker := env.Conn(t, env.WorkerURL)

	inTenant := func(conn *pgx.Conn, tenant string, fn func(tx pgx.Tx) error) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
			t.Fatal(err)
		}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	exec := func(stmt string) func(pgx.Tx) error {
		return func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt); return err }
	}
	mustFail := func(err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}

	// Direct writes the api may no longer do.
	for _, stmt := range []string{
		`UPDATE monitor_location_state SET lease_token = NULL, lease_until = NULL`,
		`UPDATE monitor_location_state SET next_run_at = now()`,
		`UPDATE monitor_location_state SET status = 'unknown'`,
		`UPDATE monitor_state SET last_checked_at = now()`,
		`SELECT 1 FROM monitor_location_state FOR UPDATE`,
	} {
		mustFail(inTenant(api, tenantA, exec(stmt)), "permission denied")
	}
	// The columns it still writes itself.
	if err := inTenant(api, tenantA, exec(`UPDATE monitor_state SET status = 'unknown', status_changed_at = now(),
		last_error_kind = NULL, tls_expires_at = NULL, tls_warned_not_after = NULL, current_incident_id = NULL`)); err != nil {
		t.Fatalf("api column grants: %v", err)
	}
	if err := inTenant(api, tenantA, exec(`SELECT 1 FROM monitor_state FOR UPDATE`)); err != nil {
		t.Fatalf("api locks monitor_state: %v", err)
	}

	reset := func(conn *pgx.Conn, tenant, monitor string) (int, error) {
		var n int
		err := inTenant(conn, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT reset_monitor_runtime($1)`, monitor).Scan(&n)
		})
		return n, err
	}
	_, err := reset(worker, tenantA, monitorA)
	mustFail(err, "permission denied")
	_, err = reset(api, "", monitorA)
	mustFail(err, "app.tenant_id is not set")
	// Another tenant's monitor is not touched.
	if n, err := reset(api, tenantA, monitorB); err != nil || n != 0 {
		t.Fatalf("reset of another tenant's monitor: n=%d err=%v", n, err)
	}
	if n, err := reset(api, tenantA, monitorA); err != nil || n != 1 {
		t.Fatalf("reset: n=%d err=%v", n, err)
	}

	type sched struct {
		status   string
		failures int
		leased   bool
		delay    time.Duration
	}
	read := func(monitor string) sched {
		var s sched
		if err := admin.QueryRow(ctx, `SELECT status, consecutive_failures, lease_token IS NOT NULL, next_run_at - now()
			FROM monitor_location_state WHERE monitor_id = $1`, monitor).Scan(&s.status, &s.failures, &s.leased, &s.delay); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := read(monitorA); s.status != "unknown" || s.failures != 0 || s.leased || s.delay < -time.Second || s.delay > 30*time.Second {
		t.Fatalf("after reset: %+v", s)
	}
	if s := read(monitorB); s.status != "down" || s.failures != 3 || !s.leased {
		t.Fatalf("other tenant changed: %+v", s)
	}
}
