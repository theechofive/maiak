package migrations_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/results"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// seedMonitor creates a monitor with its location, schedule and state rows as the superuser.
// nextRun is an SQL expression for next_run_at.
func seedMonitor(t *testing.T, admin *pgx.Conn, tenant, nextRun string, paused, deleted bool) string {
	t.Helper()
	id := uuid.New().String()
	deletedAt := "NULL"
	if deleted {
		deletedAt = "now()"
	}
	pgtest.MustExec(t, admin,
		fmt.Sprintf(`INSERT INTO monitors (id, tenant_id, name, config, paused, deleted_at)
			VALUES ('%s', '%s', 'm', '{"url":"https://example.com/%s","expectedStatus":"204"}', %t, %s)`, id, tenant, id, paused, deletedAt),
		fmt.Sprintf(`INSERT INTO monitor_locations (tenant_id, monitor_id, location_id) VALUES ('%s', '%s', '%s')`, tenant, id, homeLocation),
		fmt.Sprintf(`INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at) VALUES ('%s', '%s', '%s', %s)`, tenant, id, homeLocation, nextRun),
		fmt.Sprintf(`INSERT INTO monitor_state (tenant_id, monitor_id) VALUES ('%s', '%s')`, tenant, id),
	)
	return id
}

// monthPartition creates the check_results partition of the UTC month starting at start the way
// ensure_partitions does, for months that function does not create (the past).
func monthPartition(t *testing.T, admin *pgx.Conn, start time.Time) string {
	t.Helper()
	name := "check_results_p" + start.Format("200601")
	ident := pgx.Identifier{name}.Sanitize()
	pgtest.MustExec(t, admin,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF check_results FOR VALUES FROM ('%s') TO ('%s')`,
			ident, start.Format(time.RFC3339), start.AddDate(0, 1, 0).Format(time.RFC3339)),
		`ALTER TABLE `+ident+` OWNER TO maiak_owner`,
		`ALTER TABLE `+ident+` ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE `+ident+` FORCE ROW LEVEL SECURITY`,
		`DROP POLICY IF EXISTS tenant_isolation ON `+ident,
		`CREATE POLICY tenant_isolation ON `+ident+`
			USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
			WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)`,
	)
	return name
}

func partitions(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, _ := conn.Query(context.Background(), `SELECT c.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'check_results'::regclass ORDER BY 1`)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func monthStart(t time.Time, addMonths int) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, addMonths, 0)
}

// TestIntegrationResults covers partitions, RLS on the parent and every child, rollups,
// retention and the session purge.
func TestIntegrationResults(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("results"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	worker := env.Conn(t, env.WorkerURL)
	api := env.Conn(t, env.APIURL)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	m1 := seedMonitor(t, admin, tenantA, "now() + interval '1 hour'", false, false)
	m2 := seedMonitor(t, admin, tenantA, "now() + interval '1 hour'", false, false)

	// The database clock, so the test does not depend on the host clock.
	var dbNow time.Time
	if err := admin.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	cur := monthStart(dbNow, 0)

	t.Run("migration created three monthly partitions with RLS and grants", func(t *testing.T) {
		want := []string{"check_results_p" + cur.Format("200601"), "check_results_p" + monthStart(dbNow, 1).Format("200601"),
			"check_results_p" + monthStart(dbNow, 2).Format("200601")}
		if got := partitions(t, admin); !slices.Equal(got, want) {
			t.Fatalf("partitions = %v, want %v", got, want)
		}
		var created int
		if err := worker.QueryRow(ctx, `SELECT ensure_partitions()`).Scan(&created); err != nil || created != 0 {
			t.Fatalf("second ensure_partitions created %d (err %v), want 0", created, err)
		}
		for _, p := range want {
			var rls, force, apiIns, workerIns, workerUpd bool
			var policies int
			var owner string
			if err := admin.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity, pg_get_userbyid(c.relowner)::text,
					(SELECT count(*) FROM pg_policy WHERE polrelid = c.oid),
					has_table_privilege('maiak_api', c.oid, 'INSERT'), has_table_privilege('maiak_worker', c.oid, 'INSERT'),
					has_table_privilege('maiak_worker', c.oid, 'UPDATE')
				FROM pg_class c WHERE c.relname = $1`, p).Scan(&rls, &force, &owner, &policies, &apiIns, &workerIns, &workerUpd); err != nil {
				t.Fatal(err)
			}
			if !rls || !force || policies != 1 || owner != "maiak_owner" || apiIns || !workerIns || workerUpd {
				t.Errorf("%s: rls=%t force=%t policies=%d owner=%s apiInsert=%t workerInsert=%t workerUpdate=%t",
					p, rls, force, policies, owner, apiIns, workerIns, workerUpd)
			}
		}
	})

	t.Run("worker inserts at month boundaries under RLS", func(t *testing.T) {
		next, after := monthStart(dbNow, 1), monthStart(dbNow, 2)
		times := []time.Time{next.Add(-time.Microsecond), next, after.Add(-time.Microsecond), after}
		tx, err := worker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantA); err != nil {
			t.Fatal(err)
		}
		for _, ts := range times {
			if _, err := tx.Exec(ctx, `INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, status_code, latency_ms)
				VALUES ($1, $2, $3, $4, true, 200, 42)`, tenantA, m1, homeLocation, ts); err != nil {
				t.Fatalf("insert at %s: %v", ts, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for _, ts := range times {
			var part string
			if err := admin.QueryRow(ctx, `SELECT tableoid::regclass::text FROM check_results WHERE monitor_id = $1 AND checked_at = $2`,
				m1, ts).Scan(&part); err != nil {
				t.Fatal(err)
			}
			if want := "check_results_p" + ts.UTC().Format("200601"); part != want {
				t.Errorf("row at %s landed in %s, want %s", ts, part, want)
			}
		}

		// Tenant isolation through the parent and directly on a child; append-only grants.
		child := pgx.Identifier{"check_results_p" + next.Format("200601")}.Sanitize()
		for _, tenant := range []string{tenantB, ""} {
			for _, table := range []string{"check_results", child} {
				n := countInTenant(t, worker, tenant, `SELECT count(*) FROM `+table)
				if n != 0 {
					t.Errorf("tenant %q sees %d rows in %s", tenant, n, table)
				}
			}
		}
		if n := countInTenant(t, worker, tenantA, `SELECT count(*) FROM `+child); n != 2 {
			t.Errorf("tenant A sees %d rows in %s, want 2", n, child)
		}
		for conn, stmt := range map[*pgx.Conn]string{
			api:    `INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok) VALUES ('` + tenantA + `', '` + m1 + `', '` + homeLocation + `', now(), true)`,
			worker: `UPDATE check_results SET ok = false`,
		} {
			if _, err := execInTenant(t, conn, tenantA, stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s: err = %v, want permission denied", stmt, err)
			}
		}
		if _, err := execInTenant(t, worker, tenantB, `INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok)
			VALUES ('`+tenantA+`', '`+m1+`', '`+homeLocation+`', now(), true)`); err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Errorf("cross-tenant insert: err = %v, want row-level security violation", err)
		}
	})

	t.Run("compute_rollups is idempotent and builds the histogram", func(t *testing.T) {
		hour := dbNow.UTC().Truncate(time.Hour).Add(-2 * time.Hour)
		monthPartition(t, admin, monthStart(hour, 0))
		var latencies []int
		for _, e := range results.LatencyBucketEdgesMs {
			latencies = append(latencies, e, e+1)
		}
		latencies = append(latencies, 0)
		want := make([]int32, results.LatencyBuckets)
		sum := 0
		for i, ms := range latencies {
			want[results.Bucket(ms)]++
			sum += ms
			pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, status_code, error_kind, latency_ms)
				VALUES ('%s', '%s', '%s', '%s'::timestamptz + make_interval(secs => %d), %t, 200, %s, %d)`,
				tenantA, m1, homeLocation, hour.Format(time.RFC3339), i, i%5 != 0, map[bool]string{true: "NULL", false: "'unexpected_status'"}[i%5 != 0], ms))
		}
		// m2: two failures without a response (no latency) and one fast success.
		pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, error_kind, latency_ms) VALUES
			('%[1]s', '%[2]s', '%[3]s', '%[4]s'::timestamptz + interval '1 s', false, 'timeout', NULL),
			('%[1]s', '%[2]s', '%[3]s', '%[4]s'::timestamptz + interval '2 s', false, 'dns', NULL),
			('%[1]s', '%[2]s', '%[3]s', '%[4]s'::timestamptz + interval '3 s', true, NULL, 7)`, tenantA, m2, homeLocation, hour.Format(time.RFC3339)))
		// A result in the open hour is never rolled up.
		pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, latency_ms)
			VALUES ('%s', '%s', '%s', date_trunc('hour', now()) + interval '1 microsecond', true, 5)`, tenantA, m1, homeLocation))

		type rollup struct {
			monitor          string
			total, ok        int
			latencySum       int64
			histogram        []int32
			hour             time.Time
			histogramEntries int32
		}
		snapshot := func() []rollup {
			rows, _ := admin.Query(ctx, `SELECT monitor_id::text, total, ok_count, latency_sum_ms, latency_histogram, hour
				FROM result_rollups_hourly ORDER BY monitor_id, hour`)
			out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rollup, error) {
				var x rollup
				err := r.Scan(&x.monitor, &x.total, &x.ok, &x.latencySum, &x.histogram, &x.hour)
				for _, v := range x.histogram {
					x.histogramEntries += v
				}
				return x, err
			})
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		run := func() int {
			var n int
			if err := worker.QueryRow(ctx, `SELECT compute_rollups($1, now() + interval '1 hour')`, hour.Add(-time.Hour)).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if n := run(); n != 2 {
			t.Fatalf("first run wrote %d rollups, want 2", n)
		}
		first := snapshot()
		if n := run(); n != 2 {
			t.Fatalf("second run wrote %d rollups, want 2", n)
		}
		second := snapshot()
		if fmt.Sprint(first) != fmt.Sprint(second) {
			t.Fatalf("rollups changed between runs:\n%v\n%v", first, second)
		}
		if len(second) != 2 {
			t.Fatalf("rollups = %v, want one per monitor (open hour excluded)", second)
		}
		for _, r := range second {
			if !r.hour.Equal(hour) {
				t.Errorf("rollup hour %s, want %s", r.hour, hour)
			}
			switch r.monitor {
			case m1:
				if r.total != len(latencies) || int(r.histogramEntries) != r.total || r.latencySum != int64(sum) ||
					!slices.Equal(r.histogram, want) || r.ok != len(latencies)-(len(latencies)+4)/5 {
					t.Errorf("m1 rollup = %+v, want total %d, sum %d, histogram %v", r, len(latencies), sum, want)
				}
			case m2:
				wantHist := make([]int32, results.LatencyBuckets)
				wantHist[0] = 1
				if r.total != 3 || r.ok != 1 || r.latencySum != 7 || !slices.Equal(r.histogram, wantHist) {
					t.Errorf("m2 rollup = %+v; latencies without a response must not enter the histogram", r)
				}
			}
		}
		// A range that ends before it starts, or in the open hour only, writes nothing.
		var n int
		if err := worker.QueryRow(ctx, `SELECT compute_rollups(date_trunc('hour', now()), now() + interval '2 hours')`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("open-hour rollup wrote %d (err %v)", n, err)
		}
		if _, err := api.Exec(ctx, `SELECT compute_rollups(now() - interval '1 day', now())`); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("api executing compute_rollups: err = %v, want permission denied", err)
		}
	})

	t.Run("drop_expired_partitions keeps partitions with unrolled data", func(t *testing.T) {
		empty := monthPartition(t, admin, monthStart(dbNow, -5))
		unrolled := monthPartition(t, admin, monthStart(dbNow, -4))
		rolled := monthPartition(t, admin, monthStart(dbNow, -3))
		insertAt := func(start time.Time, secs int) {
			pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, latency_ms)
				VALUES ('%s', '%s', '%s', '%s'::timestamptz + make_interval(secs => %d), true, 30)`,
				tenantA, m1, homeLocation, start.Format(time.RFC3339), secs))
		}
		rollup := func(start time.Time) {
			if _, err := worker.Exec(ctx, `SELECT compute_rollups($1, $2)`, start, start.AddDate(0, 1, 0)); err != nil {
				t.Fatal(err)
			}
		}
		drop := func() int {
			var n int
			if err := worker.QueryRow(ctx, `SELECT drop_expired_partitions()`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		m4, m3 := monthStart(dbNow, -4), monthStart(dbNow, -3)
		for i := range 3 {
			insertAt(m4, 3600*i)
			insertAt(m3, 3600*i)
		}
		rollup(m3)
		drop()
		got := partitions(t, admin)
		if slices.Contains(got, empty) || slices.Contains(got, rolled) || !slices.Contains(got, unrolled) {
			t.Fatalf("after drop: %v; want %s and %s dropped, %s kept", got, empty, rolled, unrolled)
		}
		for i := range 3 {
			if !slices.Contains(got, "check_results_p"+monthStart(dbNow, i).Format("200601")) {
				t.Fatalf("after drop: %v; current and future partitions must stay", got)
			}
		}
		// Rolled up, then a late row arrives: the totals no longer match, so it is kept.
		rollup(m4)
		insertAt(m4, 10)
		if drop(); !slices.Contains(partitions(t, admin), unrolled) {
			t.Fatal("partition with a late, unrolled row was dropped")
		}
		rollup(m4)
		if drop(); slices.Contains(partitions(t, admin), unrolled) {
			t.Fatal("fully rolled-up expired partition was kept")
		}
		var kept int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM result_rollups_hourly WHERE hour < $1`, m3.AddDate(0, 1, 0)).Scan(&kept); err != nil || kept != 6 {
			t.Fatalf("rollups of dropped partitions: %d (err %v), want 6 kept", kept, err)
		}
	})

	t.Run("purge_expired_sessions", func(t *testing.T) {
		user := uuid.New().String()
		pgtest.MustExec(t, admin,
			`INSERT INTO users (id, email) VALUES ('`+user+`', 'u@example.com')`,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ('`+tenantA+`', '`+user+`', 'owner')`,
			`INSERT INTO sessions (id, user_id, tenant_id, token_hash, csrf_token, expires_at) VALUES
				(gen_random_uuid(), '`+user+`', '`+tenantA+`', sha256('live'), 'c', now() + interval '1 hour'),
				(gen_random_uuid(), '`+user+`', '`+tenantA+`', sha256('dead'), 'c', now() - interval '1 second')`)
		var n, left int
		if err := worker.QueryRow(ctx, `SELECT purge_expired_sessions()`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("purged %d (err %v), want 1", n, err)
		}
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash = sha256('live')`).Scan(&left); err != nil || left != 1 {
			t.Fatalf("live session gone: %d %v", left, err)
		}
		if _, err := api.Exec(ctx, `SELECT purge_expired_sessions()`); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("api executing purge_expired_sessions: err = %v, want permission denied", err)
		}
	})
}

// TestIntegrationClaimDueMonitors checks claim_due_monitors: concurrent claimers never get the
// same row, expired leases are reclaimed, paused, deleted and not-yet-due rows are skipped.
func TestIntegrationClaimDueMonitors(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("claim"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	due := map[string]bool{}
	for i := range 30 {
		tenant := tenantA
		if i%3 == 0 {
			tenant = tenantB
		}
		due[seedMonitor(t, admin, tenant, fmt.Sprintf("now() - make_interval(secs => %d)", i+1), false, false)] = true
	}
	skipped := []string{
		seedMonitor(t, admin, tenantA, "now() - interval '1 minute'", true, false),
		seedMonitor(t, admin, tenantA, "now() - interval '1 minute'", false, true),
		seedMonitor(t, admin, tenantA, "now() + interval '1 minute'", false, false),
	}

	type claim struct {
		tenant, monitor, token, url, expected string
		threshold, interval, timeout          int
		leaseUntil, nextRun                   time.Time
	}
	claimOn := func(q interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}, n int) []claim {
		rows, _ := q.Query(ctx, `SELECT tenant_id::text, monitor_id::text, lease_token::text, url, expected_status, failure_threshold,
			interval_seconds, timeout_seconds, lease_until, next_run_at FROM claim_due_monitors($1)`, n)
		out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claim, error) {
			var c claim
			err := r.Scan(&c.tenant, &c.monitor, &c.token, &c.url, &c.expected, &c.threshold, &c.interval, &c.timeout, &c.leaseUntil, &c.nextRun)
			return c, err
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return out
	}
	free := func() {
		pgtest.MustExec(t, admin, `UPDATE monitor_location_state SET lease_token = NULL, lease_until = NULL`)
	}

	w1, w2 := env.Conn(t, env.WorkerURL), env.Conn(t, env.WorkerURL)

	t.Run("two open transactions claim disjoint rows", func(t *testing.T) {
		tx1, err := w1.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		a := claimOn(tx1, 10)
		tx2, err := w2.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		b := claimOn(tx2, 100)
		if len(a) != 10 || len(b) != 20 {
			t.Fatalf("claimed %d and %d, want 10 and 20", len(a), len(b))
		}
		seen := map[string]bool{}
		for _, c := range append(a, b...) {
			if seen[c.monitor] || !due[c.monitor] {
				t.Fatalf("monitor %s claimed twice or not due", c.monitor)
			}
			seen[c.monitor] = true
		}
		// Oldest first.
		for i := 1; i < len(a); i++ {
			if a[i].nextRun.Before(a[i-1].nextRun) {
				t.Fatal("claims are not ordered by next_run_at")
			}
		}
		c := a[0]
		if c.url != "https://example.com/"+c.monitor || c.expected != "204" || c.threshold != 2 || c.interval != 60 || c.timeout != 10 {
			t.Errorf("claim row = %+v", c)
		}
		if d := c.leaseUntil.Sub(time.Now()); d < 20*time.Second || d > 40*time.Second {
			t.Errorf("lease_until is %s from now, want about 30 s", d)
		}
		if err := tx1.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx2.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if again := claimOn(w1, 100); len(again) != 0 {
			t.Fatalf("leased rows claimed again: %d", len(again))
		}
	})

	t.Run("concurrent claim loops get every row exactly once", func(t *testing.T) {
		free()
		var mu sync.Mutex
		count := map[string]int{}
		var wg sync.WaitGroup
		for _, conn := range []*pgx.Conn{w1, w2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					got := claimOn(conn, 3)
					if len(got) == 0 {
						return
					}
					mu.Lock()
					for _, c := range got {
						count[c.monitor]++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(count) != len(due) {
			t.Fatalf("claimed %d distinct monitors, want %d", len(count), len(due))
		}
		for m, n := range count {
			if n != 1 {
				t.Errorf("monitor %s claimed %d times", m, n)
			}
		}
	})

	t.Run("expired lease is reclaimed with a new token", func(t *testing.T) {
		var victim, oldToken string
		if err := admin.QueryRow(ctx, `UPDATE monitor_location_state SET lease_until = now() - interval '1 second'
			WHERE monitor_id = (SELECT monitor_id FROM monitor_location_state WHERE lease_token IS NOT NULL LIMIT 1)
			RETURNING monitor_id::text, lease_token::text`).Scan(&victim, &oldToken); err != nil {
			t.Fatal(err)
		}
		got := claimOn(w1, 100)
		if len(got) != 1 || got[0].monitor != victim || got[0].token == oldToken {
			t.Fatalf("reclaim = %+v, want only %s with a new token", got, victim)
		}
	})

	t.Run("paused, deleted and future rows are never claimed", func(t *testing.T) {
		free()
		got := claimOn(w1, 1000)
		if len(got) != len(due) {
			t.Fatalf("claimed %d, want %d", len(got), len(due))
		}
		for _, c := range got {
			if slices.Contains(skipped, c.monitor) {
				t.Fatalf("claimed skipped monitor %s", c.monitor)
			}
		}
	})

	t.Run("api may not claim", func(t *testing.T) {
		api := env.Conn(t, env.APIURL)
		if _, err := api.Exec(ctx, `SELECT * FROM claim_due_monitors(1)`); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("api claim: err = %v, want permission denied", err)
		}
	})
}

func execInTenant(t *testing.T, conn *pgx.Conn, tenant, stmt string) (int64, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, stmt)
	return tag.RowsAffected(), err
}

func countInTenant(t *testing.T, conn *pgx.Conn, tenant, query string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
