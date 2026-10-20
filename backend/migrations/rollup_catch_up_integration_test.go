package migrations_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// compute_rollups_catch_up rolls up from its watermark, at most p_max_hours per call, so a
// 10-hour gap with a cap of 6 is caught up in two runs (ADR-0090).
func TestIntegrationRollupCatchUp(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("rollupcatchup"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	worker := env.Conn(t, env.WorkerURL)
	// An odd session offset must not shift the hours: the function pins UTC.
	pgtest.MustExec(t, admin, `SET TimeZone = 'UTC'`, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A')`)
	pgtest.MustExec(t, worker, `SET TimeZone = 'Asia/Kolkata'`)
	m := seedMonitor(t, admin, tenantA, "now() + interval '1 hour'", false, false)
	var dbNow time.Time
	if err := admin.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	hour0 := dbNow.UTC().Truncate(time.Hour) // start of the open hour
	gapStart := hour0.Add(-10 * time.Hour)
	monthPartition(t, admin, monthStart(gapStart, 0))
	for i := range 10 {
		at := gapStart.Add(time.Duration(i)*time.Hour + 30*time.Minute)
		pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, latency_ms)
			VALUES ('%s', '%s', '%s', '%s', true, 20)`, tenantA, m, homeLocation, at.Format(time.RFC3339)))
	}
	// A result in the open hour is never rolled up.
	pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, latency_ms)
		VALUES ('%s', '%s', '%s', '%s', true, 20)`, tenantA, m, homeLocation, hour0.Add(time.Second).Format(time.RFC3339Nano)))

	catchUp := func(maxHours, lookback int) int {
		t.Helper()
		var n int
		if err := worker.QueryRow(ctx, `SELECT compute_rollups_catch_up($1, $2)`, maxHours, lookback).Scan(&n); err != nil {
			t.Fatalf("compute_rollups_catch_up(%d, %d): %v", maxHours, lookback, err)
		}
		return n
	}
	watermark := func() time.Time {
		t.Helper()
		var w time.Time
		if err := admin.QueryRow(ctx, `SELECT watermark FROM rollup_state`).Scan(&w); err != nil {
			t.Fatal(err)
		}
		return w.UTC()
	}
	rolled := func() []string {
		t.Helper()
		rows, _ := admin.Query(ctx, `SELECT to_char(hour AT TIME ZONE 'UTC', 'HH24') FROM result_rollups_hourly WHERE monitor_id = $1 ORDER BY hour`, m)
		h, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	hours := func(from, to time.Time) []string {
		var out []string
		for h := from; h.Before(to); h = h.Add(time.Hour) {
			out = append(out, h.Format("15"))
		}
		return out
	}
	setWatermark := func(w time.Time) {
		t.Helper()
		if _, err := admin.Exec(ctx, `INSERT INTO rollup_state (watermark) VALUES ($1)
			ON CONFLICT (singleton) DO UPDATE SET watermark = EXCLUDED.watermark`, w); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("first run starts at the earliest raw hour", func(t *testing.T) {
		if n := catchUp(1, 0); n != 1 {
			t.Fatalf("written %d, want the one earliest hour", n)
		}
		if got, want := watermark(), gapStart.Add(time.Hour); !got.Equal(want) {
			t.Fatalf("watermark %s, want %s", got, want)
		}
		pgtest.MustExec(t, admin, `DELETE FROM result_rollups_hourly`)
	})

	t.Run("a 10-hour gap is caught up in two runs", func(t *testing.T) {
		setWatermark(gapStart)
		if n := catchUp(6, 3); n != 6 {
			t.Fatalf("first run wrote %d rollups, want 6", n)
		}
		if got, want := strings.Join(rolled(), ","), strings.Join(hours(gapStart, gapStart.Add(6*time.Hour)), ","); got != want {
			t.Fatalf("after the first run: hours %s, want %s", got, want)
		}
		if got, want := watermark(), gapStart.Add(6*time.Hour); !got.Equal(want) {
			t.Fatalf("watermark %s, want %s", got, want)
		}
		if n := catchUp(6, 3); n != 4 {
			t.Fatalf("second run wrote %d rollups, want 4", n)
		}
		if got, want := strings.Join(rolled(), ","), strings.Join(hours(gapStart, hour0), ","); got != want {
			t.Fatalf("after the second run: hours %s, want %s", got, want)
		}
		if got := watermark(); !got.Equal(hour0) {
			t.Fatalf("watermark %s, want the open hour %s", got, hour0)
		}
		// Caught up: the next run only recomputes the lookback, with the same numbers.
		if n := catchUp(6, 3); n != 3 {
			t.Fatalf("caught-up run wrote %d rollups, want the 3 lookback hours", n)
		}
		var total int
		if err := admin.QueryRow(ctx, `SELECT sum(total) FROM result_rollups_hourly WHERE monitor_id = $1`, m).Scan(&total); err != nil || total != 10 {
			t.Fatalf("rolled-up results %d (err %v), want 10", total, err)
		}
	})

	t.Run("the watermark is never older than 7 days", func(t *testing.T) {
		setWatermark(hour0.AddDate(0, 0, -30))
		catchUp(6, 0)
		if got, want := watermark(), hour0.AddDate(0, 0, -7).Add(6*time.Hour); !got.Equal(want) {
			t.Fatalf("watermark %s, want %s", got, want)
		}
		setWatermark(hour0)
	})

	t.Run("arguments are bounded", func(t *testing.T) {
		for _, args := range [][2]int{{0, 3}, {169, 3}, {6, -1}, {6, 25}} {
			tx, err := worker.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `SELECT compute_rollups_catch_up($1, $2)`, args[0], args[1])
			_ = tx.Rollback(ctx)
			if err == nil || !strings.Contains(err.Error(), "22023") {
				t.Errorf("compute_rollups_catch_up(%d, %d): err = %v, want 22023", args[0], args[1], err)
			}
		}
	})

	t.Run("runtime roles cannot read or write the watermark", func(t *testing.T) {
		for name, conn := range map[string]*pgx.Conn{"maiak_worker": worker, "maiak_api": env.Conn(t, env.APIURL)} {
			for _, stmt := range []string{`SELECT * FROM rollup_state`, `UPDATE rollup_state SET watermark = now()`} {
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, err = tx.Exec(ctx, stmt)
				_ = tx.Rollback(ctx)
				if err == nil || !strings.Contains(err.Error(), "permission denied for table rollup_state") {
					t.Errorf("%s: %s: err = %v, want permission denied", name, stmt, err)
				}
			}
		}
	})
}

// reset_monitor_runtime leaves a soft-deleted monitor's schedule alone.
func TestIntegrationResetMonitorRuntimeSkipsDeleted(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("resetdeleted"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A')`)
	live := seedMonitor(t, admin, tenantA, "now() + interval '1 hour'", false, false)
	deleted := seedMonitor(t, admin, tenantA, "now() + interval '1 hour'", false, true)
	reset := func(id string) int {
		t.Helper()
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantA); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT reset_monitor_runtime($1)`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := reset(live); n != 1 {
		t.Fatalf("live monitor: reset %d locations, want 1", n)
	}
	if n := reset(deleted); n != 0 {
		t.Fatalf("deleted monitor: reset %d locations, want 0", n)
	}
}
