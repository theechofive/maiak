package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationHeartbeatHardening checks the P-08 migration (ADR-0096): a ping that waited for
// the row lock records a time after the wait, last_ping_at never moves backwards, and the
// stricter grace check rewrites 60.0 and refuses invalid rows loudly.
func TestIntegrationHeartbeatHardening(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("heartbeathardening"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A')`)
	id, hash := seedHeartbeat(t, admin, tenantA, "token-a", 60, 0, "")
	lastPing := func() *time.Time {
		t.Helper()
		var v *time.Time
		if err := admin.QueryRow(ctx, `SELECT last_ping_at FROM heartbeat_tokens WHERE monitor_id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	t.Run("a ping that waits for the row lock records a time after the wait", func(t *testing.T) {
		// The evaluation holds the row lock (jobs: LockHeartbeat, then clock_timestamp()).
		worker := env.Conn(t, env.WorkerURL)
		evalTx, err := worker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = evalTx.Rollback(ctx) }()
		if _, err := evalTx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantA); err != nil {
			t.Fatal(err)
		}
		if _, err := evalTx.Exec(ctx, `SELECT 1 FROM heartbeat_tokens WHERE monitor_id = $1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}

		// The ping's transaction starts before the wait: now() would be the start of the wait.
		api := env.Conn(t, env.APIURL)
		pingTx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pingTx.Rollback(ctx) }()
		var pingStart time.Time
		var pid int
		if err := pingTx.QueryRow(ctx, `SELECT now(), pg_backend_pid()`).Scan(&pingStart, &pid); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			var recorded bool
			done <- pingTx.QueryRow(ctx, `SELECT recorded FROM heartbeat_ping($1)`, hash).Scan(&recorded)
		}()
		waitForLock(t, admin, pid)

		var evalNow time.Time
		if err := evalTx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&evalNow); err != nil {
			t.Fatal(err)
		}
		if err := evalTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := pingTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		got := lastPing()
		if got == nil || !got.After(evalNow) || !got.After(pingStart) {
			t.Fatalf("last_ping_at = %v, want after the evaluation's clock %v (ping transaction started %v)", got, evalNow, pingStart)
		}
	})

	t.Run("last_ping_at never moves backwards", func(t *testing.T) {
		future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		if _, err := admin.Exec(ctx, `UPDATE heartbeat_tokens SET last_ping_at = $2 WHERE monitor_id = $1`, id, future); err != nil {
			t.Fatal(err)
		}
		api := env.Conn(t, env.APIURL)
		var recorded bool
		if err := api.QueryRow(ctx, `SELECT recorded FROM heartbeat_ping($1)`, hash).Scan(&recorded); err != nil || !recorded {
			t.Fatalf("ping: %t %v", recorded, err)
		}
		if got := lastPing(); got == nil || !got.Equal(future) {
			t.Fatalf("last_ping_at = %v, want %v", got, future)
		}
	})

	// Replays the migration over a database whose check still accepted any grace.
	replay := func(t *testing.T, grace string) error {
		t.Helper()
		pgtest.MustExec(t, admin,
			`ALTER TABLE monitors DROP CONSTRAINT monitors_heartbeat_check`,
			`ALTER TABLE monitors ADD CONSTRAINT monitors_heartbeat_check CHECK (true)`,
			`DELETE FROM goose_db_version WHERE version_id = 20261021180000`,
			`UPDATE monitors SET config = jsonb_set(config, '{graceSeconds}', '`+grace+`'::jsonb) WHERE id = '`+id+`'`)
		n, err := env.Migrate(ctx)
		if err == nil && n != 1 {
			t.Fatalf("re-applied %d migrations, want 1", n)
		}
		return err
	}
	graceText := func() string {
		t.Helper()
		var g string
		if err := admin.QueryRow(ctx, `SELECT config ->> 'graceSeconds' FROM monitors WHERE id = $1`, id).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}

	t.Run("the migration rewrites an integral 60.0 and keeps the casts working", func(t *testing.T) {
		if err := replay(t, "60.0"); err != nil {
			t.Fatal(err)
		}
		if g := graceText(); g != "60" {
			t.Fatalf("graceSeconds = %q, want 60", g)
		}
		worker := env.Conn(t, env.WorkerURL)
		rows, err := worker.Query(ctx, `SELECT monitor_id FROM list_due_heartbeats(10)`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			t.Fatalf("list_due_heartbeats: %v", err)
		}
		if _, err := admin.Exec(ctx, `UPDATE monitors SET config = '{"graceSeconds": 60.0}' WHERE id = $1`, id); err == nil || !strings.Contains(err.Error(), "monitors_heartbeat_check") {
			t.Fatalf("60.0 after the migration: %v", err)
		}
	})

	t.Run("the migration fails loudly on invalid rows", func(t *testing.T) {
		for _, bad := range []string{`"60"`, `1.5`, `3601`, `null`} {
			err := replay(t, bad)
			if err == nil || !strings.Contains(err.Error(), "invalid configuration") || !strings.Contains(err.Error(), id) {
				t.Fatalf("grace %s: err = %v, want the invalid-configuration error naming %s", bad, err, id)
			}
			// Rolled back: the version is not recorded (the next replay starts from the same state).
			var n int
			if err := admin.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id = 20261021180000`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("version recorded after a failure: %d %v", n, err)
			}
		}
		if err := replay(t, "30"); err != nil {
			t.Fatalf("after fixing the row: %v", err)
		}
		if g := graceText(); g != "30" {
			t.Fatalf("graceSeconds = %q", g)
		}
	})
}

// waitForLock waits until backend pid is waiting for a lock.
func waitForLock(t *testing.T, admin *pgx.Conn, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := admin.QueryRow(context.Background(), `SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the ping never waited for the row lock")
}
