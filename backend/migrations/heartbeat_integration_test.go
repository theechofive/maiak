package migrations_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// seedHeartbeat inserts a heartbeat monitor with its rows the way monitors.Service.Create does
// and returns its id and token hash.
func seedHeartbeat(t *testing.T, admin *pgx.Conn, tenant, token string, interval, grace int, extra string) (string, []byte) {
	t.Helper()
	id := uuid.New().String()
	sum := sha256.Sum256([]byte(token))
	pgtest.MustExec(t, admin,
		fmt.Sprintf(`INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold)
			VALUES ('%s', '%s', 'hb', 'heartbeat', '{"graceSeconds": %d}', %d, 1)`, id, tenant, grace, interval),
		fmt.Sprintf(`INSERT INTO monitor_locations (tenant_id, monitor_id, location_id) VALUES ('%s', '%s', '%s')`, tenant, id, homeLocation),
		fmt.Sprintf(`INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at) VALUES ('%s', '%s', '%s', 'infinity')`, tenant, id, homeLocation),
		fmt.Sprintf(`INSERT INTO monitor_state (tenant_id, monitor_id) VALUES ('%s', '%s')`, tenant, id))
	if _, err := admin.Exec(context.Background(), `INSERT INTO heartbeat_tokens (tenant_id, monitor_id, token_hash) VALUES ($1, $2, $3)`, tenant, id, sum[:]); err != nil {
		t.Fatal(err)
	}
	if extra != "" {
		pgtest.MustExec(t, admin, strings.ReplaceAll(extra, "$ID", id))
	}
	return id, sum[:]
}

// TestIntegrationHeartbeatSchema checks the heartbeat migration (P-03, ADR-0094): constraints,
// grants, heartbeat_ping, list_due_heartbeats, and that workers never claim a heartbeat.
func TestIntegrationHeartbeatSchema(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("heartbeats"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	api := env.Conn(t, env.APIURL)
	worker := env.Conn(t, env.WorkerURL)

	t.Run("constraints: the MVP limits bind http only, heartbeats have their own", func(t *testing.T) {
		for stmt, want := range map[string]string{
			`INSERT INTO monitors (id, tenant_id, name, config, interval_seconds) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', '{"url":"https://example.com"}', 300)`:                                   "monitors_mvp_interval",
			`INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', 'heartbeat', '{"graceSeconds": 0}', 59, 1)`:     "monitors_heartbeat_check",
			`INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', 'heartbeat', '{"graceSeconds": 0}', 86401, 1)`:  "monitors_heartbeat_check",
			`INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', 'heartbeat', '{"graceSeconds": 0}', 300, 2)`:    "monitors_heartbeat_check",
			`INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', 'heartbeat', '{}', 300, 1)`:                     "monitors_heartbeat_check",
			`INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', 'heartbeat', '{"graceSeconds": "60"}', 300, 1)`: "monitors_heartbeat_check",
			`INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', 'heartbeat', '{"graceSeconds": 3601}', 300, 1)`: "monitors_heartbeat_check",
			`INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', 'heartbeat', '{"graceSeconds": 1.5}', 300, 1)`:  "monitors_heartbeat_check",
			`INSERT INTO monitors (id, tenant_id, name, type, config) VALUES (gen_random_uuid(), '` + tenantA + `', 'h', 'tcp', '{}')`:                                                                        "monitors_type_check",
		} {
			if _, err := admin.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %v, want %s", stmt, err, want)
			}
		}
		if _, err := admin.Exec(ctx, `INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, failure_threshold)
			VALUES (gen_random_uuid(), '`+tenantA+`', 'h', 'heartbeat', '{"graceSeconds": 0}', 86400, 1)`); err != nil {
			t.Fatalf("valid heartbeat: %v", err)
		}
	})

	hbA, hashA := seedHeartbeat(t, admin, tenantA, "token-a", 60, 0, "")
	hbB, hashB := seedHeartbeat(t, admin, tenantB, "token-b", 60, 0, "")
	paused, hashPaused := seedHeartbeat(t, admin, tenantA, "token-paused", 60, 0, `UPDATE monitors SET paused = true WHERE id = '$ID'`)
	deleted, hashDeleted := seedHeartbeat(t, admin, tenantA, "token-deleted", 60, 0, `UPDATE monitors SET deleted_at = now() WHERE id = '$ID'`)

	ping := func(hash []byte) (tenant, monitor string, recorded, found bool) {
		t.Helper()
		var tid, mid uuid.UUID
		err := api.QueryRow(ctx, `SELECT tenant_id, monitor_id, recorded FROM heartbeat_ping($1)`, hash).Scan(&tid, &mid, &recorded)
		if err == pgx.ErrNoRows {
			return "", "", false, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return tid.String(), mid.String(), recorded, true
	}
	lastPing := func(id string) *string {
		t.Helper()
		var v *string
		if err := admin.QueryRow(ctx, `SELECT last_ping_at::text FROM heartbeat_tokens WHERE monitor_id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	t.Run("heartbeat_ping", func(t *testing.T) {
		other := sha256.Sum256([]byte("never issued"))
		for name, h := range map[string][]byte{"unknown": other[:], "short": {1, 2, 3}, "empty": {}, "long": make([]byte, 64)} {
			if _, _, _, found := ping(h); found {
				t.Errorf("%s hash found a heartbeat", name)
			}
		}
		if tenant, monitor, recorded, found := ping(hashA); !found || !recorded || tenant != tenantA || monitor != hbA {
			t.Fatalf("ping A = %s %s %t %t", tenant, monitor, recorded, found)
		}
		// Tenant isolation: A's token touched A's row only.
		if lastPing(hbA) == nil || lastPing(hbB) != nil {
			t.Fatalf("last pings: A %v, B %v", lastPing(hbA), lastPing(hbB))
		}
		if tenant, monitor, _, _ := ping(hashB); tenant != tenantB || monitor != hbB {
			t.Fatalf("ping B = %s %s", tenant, monitor)
		}
		for name, c := range map[string]struct {
			id   string
			hash []byte
		}{"paused": {paused, hashPaused}, "deleted": {deleted, hashDeleted}} {
			if _, monitor, recorded, found := ping(c.hash); !found || recorded || monitor != c.id || lastPing(c.id) != nil {
				t.Errorf("%s: found %t recorded %t last ping %v", name, found, recorded, lastPing(c.id))
			}
		}
		// A suspended tenant's heartbeat is known but not recorded.
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantB+`'`)
		if _, _, recorded, found := ping(hashB); !found || recorded {
			t.Errorf("suspended tenant: found %t recorded %t", found, recorded)
		}
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'active' WHERE id = '`+tenantB+`'`)
	})

	t.Run("grants", func(t *testing.T) {
		for _, c := range []struct {
			conn *pgx.Conn
			stmt string
		}{
			{api, `UPDATE heartbeat_tokens SET last_ping_at = now()`},
			{api, `UPDATE heartbeat_tokens SET missed_deadline = now()`},
			{api, `DELETE FROM heartbeat_tokens`},
			{worker, `UPDATE heartbeat_tokens SET last_ping_at = now()`},
			{worker, `UPDATE heartbeat_tokens SET token_hash = '\x00'`},
			{worker, `INSERT INTO heartbeat_tokens (tenant_id, monitor_id, token_hash) VALUES ('` + tenantA + `', gen_random_uuid(), '\x00')`},
			{worker, `DELETE FROM heartbeat_tokens`},
		} {
			if _, err := execInTenant(t, c.conn, tenantA, c.stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s: err = %v, want permission denied", c.stmt, err)
			}
		}
		for _, c := range []struct {
			conn *pgx.Conn
			stmt string
		}{
			{api, `UPDATE heartbeat_tokens SET token_hash = token_hash, created_at = now(), waiting_since = now(), applied_ping_at = last_ping_at`},
			{api, `SELECT 1 FROM heartbeat_tokens FOR UPDATE`},
			{worker, `UPDATE heartbeat_tokens SET applied_ping_at = last_ping_at, missed_deadline = now()`},
			{worker, `SELECT 1 FROM heartbeat_tokens FOR UPDATE`},
		} {
			if _, err := execInTenant(t, c.conn, tenantA, c.stmt); err != nil {
				t.Errorf("%s: %v", c.stmt, err)
			}
		}
		// The tenant policy hides other tenants' tokens.
		if n := countInTenant(t, api, tenantA, `SELECT count(*) FROM heartbeat_tokens WHERE tenant_id = '`+tenantB+`'`); n != 0 {
			t.Fatalf("tenant A sees %d rows of B", n)
		}
	})

	t.Run("list_due_heartbeats", func(t *testing.T) {
		due := func() map[string]bool {
			t.Helper()
			rows, err := worker.Query(ctx, `SELECT monitor_id::text FROM list_due_heartbeats(100)`)
			if err != nil {
				t.Fatal(err)
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			for _, id := range ids {
				out[id] = true
			}
			return out
		}
		// Unapplied pings are due; paused and deleted heartbeats never are.
		if d := due(); !d[hbA] || !d[hbB] || d[paused] || d[deleted] || len(d) != 2 {
			t.Fatalf("due = %v", d)
		}
		pgtest.MustExec(t, admin, `UPDATE heartbeat_tokens SET applied_ping_at = last_ping_at`)
		if d := due(); len(d) != 0 {
			t.Fatalf("due after applying the pings = %v", d)
		}
		// A passed deadline is due until it is recorded as missed.
		pgtest.MustExec(t, admin, `UPDATE heartbeat_tokens SET waiting_since = now() - interval '1 hour', last_ping_at = now() - interval '61 seconds', applied_ping_at = now() - interval '61 seconds' WHERE monitor_id = '`+hbA+`'`)
		if d := due(); !d[hbA] || len(d) != 1 {
			t.Fatalf("due with a passed deadline = %v", d)
		}
		pgtest.MustExec(t, admin, `UPDATE heartbeat_tokens SET missed_deadline = last_ping_at + interval '60 seconds' WHERE monitor_id = '`+hbA+`'`)
		if d := due(); len(d) != 0 {
			t.Fatalf("due after recording the miss = %v", d)
		}
		// A never-pinged heartbeat is due once its first deadline (from waiting_since) passed.
		pgtest.MustExec(t, admin, `UPDATE heartbeat_tokens SET waiting_since = now() - interval '2 minutes', last_ping_at = NULL, applied_ping_at = NULL WHERE monitor_id = '`+hbB+`'`)
		if d := due(); !d[hbB] || len(d) != 1 {
			t.Fatalf("due never pinged = %v", d)
		}
		// Suspended tenants are skipped.
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantB+`'`)
		if d := due(); len(d) != 0 {
			t.Fatalf("due with B suspended = %v", d)
		}
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'active' WHERE id = '`+tenantB+`'`)
	})

	t.Run("workers never claim a heartbeat", func(t *testing.T) {
		pgtest.MustExec(t, admin, `UPDATE monitor_location_state SET next_run_at = now() - interval '1 hour'`)
		http := seedMonitor(t, admin, tenantA, "now() - interval '1 minute'", false, false)
		rows, err := worker.Query(ctx, `SELECT monitor_id::text FROM claim_due_monitors(100, 'home-eu-central-1')`)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) != 1 || claimed[0] != http {
			t.Fatalf("claimed %v, want only the http monitor %s", claimed, http)
		}
	})

	t.Run("reset keeps a heartbeat parked", func(t *testing.T) {
		var n int
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantA); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT reset_monitor_runtime($1)`, hbA).Scan(&n); err != nil || n != 1 {
			t.Fatalf("reset: %d %v", n, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		var next string
		if err := admin.QueryRow(ctx, `SELECT next_run_at::text FROM monitor_location_state WHERE monitor_id = $1`, hbA).Scan(&next); err != nil || next != "infinity" {
			t.Fatalf("next_run_at = %q (%v), want infinity", next, err)
		}
	})
}
