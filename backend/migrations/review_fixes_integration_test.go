package migrations_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// definerCalls calls every allow-listed SECURITY DEFINER function with harmless arguments.
var definerCalls = map[string]string{
	"auth_lookup_session":       `SELECT * FROM auth_lookup_session('\x00')`,
	"auth_login_tenant":         `SELECT auth_login_tenant(gen_random_uuid())`,
	"ensure_partitions":         `SELECT ensure_partitions()`,
	"claim_due_monitors":        `SELECT * FROM claim_due_monitors(1, 'home-eu-central-1')`,
	"compute_rollups":           `SELECT compute_rollups(now() - interval '2 hours', now())`,
	"compute_rollups_catch_up":  `SELECT compute_rollups_catch_up(1, 0)`,
	"list_droppable_partitions": `SELECT * FROM list_droppable_partitions()`,
	"detach_expired_partition":  `SELECT detach_expired_partition('check_results_p200001')`,
	"drop_detached_partition":   `SELECT drop_detached_partition('check_results_p200001')`,
	"purge_expired_sessions":    `SELECT purge_expired_sessions()`,
	"claim_deliveries":          `SELECT * FROM claim_deliveries(1)`,
	"finish_delivery":           `SELECT finish_delivery(gen_random_uuid(), gen_random_uuid(), 'sent', NULL, NULL)`,
	"outbox_pending":            `SELECT outbox_pending()`,
	"reset_monitor_runtime":     `SELECT reset_monitor_runtime(gen_random_uuid())`,
}

// Every definer function in the allow-list is executable by exactly the roles listed there: a
// runtime role that is not listed gets "permission denied", a listed one does not.
func TestIntegrationDefinerCallsByRole(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("definercalls"))
	ctx := context.Background()
	conns := map[string]*pgx.Conn{"maiak_api": env.Conn(t, env.APIURL), "maiak_worker": env.Conn(t, env.WorkerURL)}
	for name, allowed := range definerAllowList {
		stmt, ok := definerCalls[name]
		if !ok {
			t.Errorf("%s: allow-listed but has no call in definerCalls", name)
			continue
		}
		for _, role := range runtimeRoles {
			t.Run(name+"/"+role, func(t *testing.T) {
				tx, err := conns[role].Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				_, err = tx.Exec(ctx, stmt)
				denied := err != nil && strings.Contains(err.Error(), "permission denied for function "+name)
				if want := !slices.Contains(allowed, role); denied != want {
					t.Fatalf("%s as %s: err = %v, want denied=%t", stmt, role, err, want)
				}
			})
		}
	}
}

// The partial unique index allows one open automatic incident per monitor; resolved and manual
// (monitor_id NULL) incidents are not limited.
func TestIntegrationOneOpenIncidentPerMonitor(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("oneopen"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A')`)
	m := seedMonitor(t, admin, tenantA, "now()", false, false)
	insert := func(monitor string) (string, error) {
		id := uuid.New().String()
		mon := "NULL"
		if monitor != "" {
			mon = "'" + monitor + "'"
		}
		_, err := admin.Exec(ctx, fmt.Sprintf(`INSERT INTO incidents (id, tenant_id, monitor_id, started_at) VALUES ('%s', '%s', %s, now())`, id, tenantA, mon))
		return id, err
	}
	first, err := insert(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insert(m); err == nil || !strings.Contains(err.Error(), "incidents_one_open_per_monitor_idx") {
		t.Fatalf("second open incident: err = %v, want the unique index", err)
	}
	pgtest.MustExec(t, admin, `UPDATE incidents SET resolved_at = now(), resolve_reason = 'recovered' WHERE id = '`+first+`'`)
	if _, err := insert(m); err != nil {
		t.Fatalf("open incident after the first was resolved: %v", err)
	}
	for range 2 {
		if _, err := insert(""); err != nil {
			t.Fatalf("manual incident: %v", err)
		}
	}
}

// The definer functions that compute month and hour boundaries give the same result in any
// session time zone, including ones with odd offsets (ADR-0021, ADR-0022).
func TestIntegrationNonUTCSessionTimeZone(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("nonutc"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	worker := env.Conn(t, env.WorkerURL)
	// The admin session renders partition bounds in UTC whatever the server time zone is.
	pgtest.MustExec(t, admin, `SET TimeZone = 'UTC'`, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A')`)
	m := seedMonitor(t, admin, tenantA, "now() + interval '1 hour'", false, false)
	var dbNow time.Time
	if err := admin.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}

	for i, tz := range []string{"Asia/Kolkata", "Pacific/Chatham", "America/St_Johns"} {
		t.Run(tz, func(t *testing.T) {
			pgtest.MustExec(t, worker, `SET TimeZone = '`+tz+`'`)

			// ensure_partitions recreates the missing month with its UTC name and bounds.
			last := "check_results_p" + monthStart(dbNow, 2).Format("200601")
			pgtest.MustExec(t, admin, `DROP TABLE `+last)
			var created int
			if err := worker.QueryRow(ctx, `SELECT ensure_partitions()`).Scan(&created); err != nil || created != 1 {
				t.Fatalf("ensure_partitions created %d (err %v), want 1", created, err)
			}
			var bound string
			if err := admin.QueryRow(ctx, `SELECT pg_get_expr(c.relpartbound, c.oid) FROM pg_class c WHERE c.relname = $1`, last).Scan(&bound); err != nil {
				t.Fatal(err)
			}
			if lo := monthStart(dbNow, 2).Format("2006-01-02 15:04:05"); !strings.Contains(bound, lo+"+00") {
				t.Fatalf("%s bound %s; want it to start at %s UTC", last, bound, lo)
			}

			// compute_rollups buckets by UTC hour.
			hour := dbNow.UTC().Truncate(time.Hour).Add(-2 * time.Hour)
			monthPartition(t, admin, monthStart(hour, 0))
			at := hour.Add(time.Duration(10*i+5) * time.Minute)
			pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, status_code, latency_ms)
				VALUES ('%s', '%s', '%s', '%s', true, 200, 20)`, tenantA, m, homeLocation, at.Format(time.RFC3339Nano)))
			if _, err := worker.Exec(ctx, `SELECT compute_rollups($1, $2)`, hour.Add(-time.Hour), hour.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			var total int
			if err := admin.QueryRow(ctx, `SELECT total FROM result_rollups_hourly WHERE monitor_id = $1 AND hour = $2`, m, hour).Scan(&total); err != nil || total != i+1 {
				t.Fatalf("rollup of %s UTC: total %d (err %v), want %d", hour, total, err, i+1)
			}

			// Retention drops a rolled-up expired month and keeps the current and future ones.
			old := monthStart(dbNow, -3)
			name := monthPartition(t, admin, old)
			pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, latency_ms)
				VALUES ('%s', '%s', '%s', '%s', true, 30)`, tenantA, m, homeLocation, old.Add(time.Hour).Format(time.RFC3339)))
			if _, err := worker.Exec(ctx, `SELECT compute_rollups($1, $2)`, old, old.AddDate(0, 1, 0)); err != nil {
				t.Fatal(err)
			}
			if n := retention(t, worker); n < 1 {
				t.Fatalf("retention dropped %d partitions", n)
			}
			got := partitions(t, admin)
			if slices.Contains(got, name) {
				t.Fatalf("expired partition %s kept: %v", name, got)
			}
			for k := range 3 {
				if p := "check_results_p" + monthStart(dbNow, k).Format("200601"); !slices.Contains(got, p) {
					t.Fatalf("partition %s dropped: %v", p, got)
				}
			}
		})
	}
}

// Retention refuses names outside the partition pattern and never touches the current month.
func TestIntegrationRetentionFunctionsRefuseOtherTables(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("retentionguard"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	worker := env.Conn(t, env.WorkerURL)
	for _, bad := range []string{"monitors", "check_results", "check_results_p202613", "check_results_p2026011", `check_results_p202601"; DROP TABLE monitors; --`} {
		for _, fn := range []string{"detach_expired_partition", "drop_detached_partition"} {
			if _, err := worker.Exec(ctx, `SELECT `+fn+`($1)`, bad); err == nil || !strings.Contains(err.Error(), "invalid partition name") {
				t.Errorf("%s(%q): err = %v, want invalid partition name", fn, bad, err)
			}
		}
	}
	var dbNow time.Time
	if err := admin.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	cur := "check_results_p" + monthStart(dbNow, 0).Format("200601")
	for _, fn := range []string{"detach_expired_partition", "drop_detached_partition"} {
		var ok bool
		if err := worker.QueryRow(ctx, `SELECT `+fn+`($1)`, cur).Scan(&ok); err != nil || ok {
			t.Errorf("%s(current month) = %t, err %v; want false", fn, ok, err)
		}
	}
	// An attached partition is never dropped directly.
	old := monthPartition(t, admin, monthStart(dbNow, -6))
	var ok bool
	if err := worker.QueryRow(ctx, `SELECT drop_detached_partition($1)`, old).Scan(&ok); err != nil || ok {
		t.Fatalf("drop_detached_partition(attached) = %t, err %v; want false", ok, err)
	}
	if !slices.Contains(partitions(t, admin), old) {
		t.Fatal("attached partition dropped")
	}
	// A detached leftover of an interrupted run is listed again and dropped.
	pgtest.MustExec(t, admin, `ALTER TABLE check_results DETACH PARTITION `+old)
	rows, _ := worker.Query(ctx, `SELECT name || ':' || detached FROM list_droppable_partitions()`)
	listed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(listed, []string{old + ":true"}) {
		t.Fatalf("listed %v (err %v), want only the detached %s", listed, err, old)
	}
	if n := retention(t, worker); n != 1 {
		t.Fatalf("retention dropped %d, want the detached leftover", n)
	}
}
