package statuspages_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/statuspages"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const homeLocation = "01920000-0000-7000-8000-000000000001"

type fixture struct {
	t     *testing.T
	admin *pgx.Conn
}

func (f fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.admin.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// monitor inserts a live monitor with its state rows; typ is http or heartbeat.
func (f fixture) monitor(tenant, name, typ, status string, failures int, created time.Time) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	config, threshold := `{"url":"https://internal.example.net/`+name+`","expectedStatus":"2xx"}`, 2
	if typ == "heartbeat" {
		config, threshold = `{"graceSeconds":60}`, 1
	}
	f.exec(`INSERT INTO monitors (id, tenant_id, name, type, config, failure_threshold, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, tenant, name, typ, config, threshold, created)
	f.exec(`INSERT INTO monitor_locations (tenant_id, monitor_id, location_id) VALUES ($1, $2, $3)`, tenant, id, homeLocation)
	f.exec(`INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at, status, consecutive_failures) VALUES ($1, $2, $3, now(), $4, $5)`,
		tenant, id, homeLocation, status, failures)
	f.exec(`INSERT INTO monitor_state (tenant_id, monitor_id, status) VALUES ($1, $2, $3)`, tenant, id, status)
	return id
}

func (f fixture) rollup(tenant string, monitor uuid.UUID, hour time.Time, total, ok int) {
	f.t.Helper()
	f.exec(`INSERT INTO result_rollups_hourly (tenant_id, monitor_id, location_id, hour, total, ok_count, latency_sum_ms, latency_histogram)
		VALUES ($1, $2, $3, $4, $5, $6, 0, '{0,0,0,0,0,0,0,0,0,0,0}')`, tenant, monitor, homeLocation, hour, total, ok)
}

func (f fixture) incident(tenant string, monitor uuid.UUID, start time.Time, end *time.Time) {
	f.t.Helper()
	reason := any(nil)
	if end != nil {
		reason = "recovered"
	}
	f.exec(`INSERT INTO incidents (id, tenant_id, monitor_id, started_at, resolved_at, resolve_reason, reason) VALUES ($1, $2, $3, $4, $5, $6, 'timeout')`,
		uuid.New(), tenant, monitor, start, end, reason)
}

// owner is the acting user of the fixture (audit rows reference users).
var owner = uuid.New()

func ownerOf(tenant string) statuspages.Actor {
	return statuspages.Actor{TenantID: uuid.MustParse(tenant), Access: authz.Actor{UserID: owner, Role: authz.Owner}}
}

// TestIntegrationSnapshotBuildAndRebuild builds real snapshots from rollups, monitor state and
// incidents, checks the status rules end to end, tenant isolation of the rebuild, the removal of
// components of deleted monitors, and that the payload never holds internal names or URLs.
func TestIntegrationSnapshotBuildAndRebuild(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("statuspages"))
	ctx := context.Background()
	f := fixture{t: t, admin: env.AdminConn(t)}
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	f.exec(`INSERT INTO tenants (id, slug, name) VALUES ($1, 'sp-a', 'A'), ($2, 'sp-b', 'B')`, tenantA, tenantB)
	f.exec(`INSERT INTO users (id, email) VALUES ($1, 'owner@example.com')`, owner)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	long := today.AddDate(0, 0, -200)
	api1 := f.monitor(tenantA, "secret-api-name", "http", "up", 0, long)
	web := f.monitor(tenantA, "secret-web-name", "http", "up", 1, long)       // failing below threshold
	dbm := f.monitor(tenantA, "secret-db-name", "http", "down", 2, long)      // down
	hb := f.monitor(tenantA, "secret-cron-name", "heartbeat", "up", 0, long)  // heartbeat
	gone := f.monitor(tenantA, "secret-gone-name", "http", "up", 0, long)     // deleted below
	other := f.monitor(tenantB, "secret-other-name", "http", "down", 2, long) // tenant B
	yesterday := today.AddDate(0, 0, -1)
	for h := range 24 {
		f.rollup(tenantA, api1, yesterday.Add(time.Duration(h)*time.Hour), 60, 60)
		ok := 60
		if h < 10 {
			ok = 59 // 10 failures of 1440: 99.3 %, a degraded day
		}
		f.rollup(tenantA, web, yesterday.Add(time.Duration(h)*time.Hour), 60, ok)
	}
	f.rollup(tenantA, dbm, yesterday, 60, 0)
	resolved := yesterday.Add(2 * time.Hour)
	f.incident(tenantA, dbm, yesterday.Add(time.Hour), &resolved)
	f.incident(tenantA, dbm, now.Add(-10*time.Minute), nil)
	hbEnd := yesterday.Add(13 * time.Hour)
	f.incident(tenantA, hb, yesterday.Add(12*time.Hour), &hbEnd)
	f.incident(tenantB, other, now.Add(-time.Hour), nil)

	pool := env.Pool(t, env.APIURL, 4)
	clk := clock.NewFake(now)
	svc := statuspages.NewService(pool, statuspages.Options{Clock: clk, HashParams: &crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}})
	a, b := ownerOf(tenantA), ownerOf(tenantB)
	pageA, err := svc.Create(ctx, a, statuspages.CreateInput{Slug: "page-a", Title: "A <script>alert(1)</script>"})
	if err != nil {
		t.Fatal(err)
	}
	pageB, err := svc.Create(ctx, b, statuspages.CreateInput{Slug: "page-b", Title: "B"})
	if err != nil {
		t.Fatal(err)
	}
	core := "Core"
	if _, err := svc.PutComponents(ctx, a, pageA.ID, []statuspages.ComponentInput{
		{MonitorID: api1, PublicName: "API", GroupName: &core}, {MonitorID: web, PublicName: "Website", GroupName: &core},
		{MonitorID: dbm, PublicName: "Database"}, {MonitorID: hb, PublicName: "Nightly job"}, {MonitorID: gone, PublicName: "Gone"},
	}); err != nil {
		t.Fatal(err)
	}
	// Tenant B cannot use tenant A's monitor.
	if _, err := svc.PutComponents(ctx, b, pageB.ID, []statuspages.ComponentInput{{MonitorID: api1, PublicName: "Stolen"}}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("cross-tenant component: %v", err)
	}
	if _, err := svc.PutComponents(ctx, b, pageB.ID, []statuspages.ComponentInput{{MonitorID: other, PublicName: "B service"}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	for _, x := range []struct {
		a  statuspages.Actor
		id uuid.UUID
	}{{a, pageA.ID}, {b, pageB.ID}} {
		if _, err := svc.Update(ctx, x.a, x.id, statuspages.UpdateInput{Published: &yes}); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(`UPDATE monitors SET deleted_at = now() WHERE id = $1`, gone)

	// jobs rebuilds as maiak_worker.
	jobs := statuspages.NewService(env.Pool(t, env.WorkerURL, 4), statuspages.Options{Clock: clk})
	n, err := jobs.RebuildAll(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RebuildAll = %d, %v", n, err)
	}

	public := env.Conn(t, env.StatuspageURL)
	read := func(slug string) statuspages.Snapshot {
		t.Helper()
		var raw []byte
		if err := public.QueryRow(ctx, `SELECT payload FROM status_page_snapshot($1)`, slug).Scan(&raw); err != nil {
			t.Fatalf("snapshot %s: %v", slug, err)
		}
		for _, banned := range []string{"secret-", "internal.example.net", "timeout", tenantA, tenantB, api1.String(), pageA.ID.String()} {
			if strings.Contains(string(raw), banned) {
				t.Fatalf("snapshot %s leaks %q: %s", slug, banned, raw)
			}
		}
		var s statuspages.Snapshot
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	sa := read("page-a")
	if len(sa.Components) != 4 {
		t.Fatalf("components (deleted monitor dropped): %+v", sa.Components)
	}
	want := map[string]statuspages.Status{"API": statuspages.Operational, "Website": statuspages.Degraded, "Database": statuspages.Outage, "Nightly job": statuspages.Operational}
	for _, c := range sa.Components {
		if c.Status != want[c.Name] || len(c.Days) != 90 {
			t.Errorf("%s: %s (%d days), want %s", c.Name, c.Status, len(c.Days), want[c.Name])
		}
	}
	if sa.Status != statuspages.PartialOutage || sa.Page.Title != "A <script>alert(1)</script>" {
		t.Errorf("overall %s, title %q", sa.Status, sa.Page.Title)
	}
	day := func(c statuspages.Component) statuspages.Day { return c.Days[88] }
	if d := day(sa.Components[0]); d.Status != statuspages.Operational || *d.Uptime != 100 {
		t.Errorf("API yesterday: %+v", d)
	}
	if d := day(sa.Components[1]); d.Status != statuspages.Degraded {
		t.Errorf("Website yesterday (%v): %+v", *d.Uptime, d)
	}
	if d := day(sa.Components[2]); d.Status != statuspages.Outage {
		t.Errorf("Database yesterday: %+v", d)
	}
	if d := day(sa.Components[3]); d.Uptime == nil || *d.Uptime != 95.833 || d.Status != statuspages.Outage {
		t.Errorf("heartbeat yesterday from incidents: %+v", d)
	}
	if *sa.Components[0].Group != "Core" || sa.Components[2].Group != nil {
		t.Error("groups")
	}
	if len(sa.ActiveIncidents) != 1 || sa.ActiveIncidents[0].Title != "Database is unavailable" || len(sa.PastIncidents) != 2 {
		t.Fatalf("incidents: %+v / %+v", sa.ActiveIncidents, sa.PastIncidents)
	}
	if sa.PastIncidents[0].DurationSeconds != 3600 || sa.PastIncidents[0].Title != "Nightly job is unavailable" {
		t.Errorf("past incidents newest first: %+v", sa.PastIncidents)
	}

	// Tenant B's snapshot holds only its own component and incident.
	sb := read("page-b")
	if len(sb.Components) != 1 || sb.Components[0].Name != "B service" || len(sb.ActiveIncidents) != 1 || sb.Status != statuspages.MajorOutage {
		t.Fatalf("page B: %+v", sb)
	}

	// A suspended tenant's page is neither rebuilt nor served.
	f.exec(`UPDATE tenants SET status = 'suspended' WHERE id = $1`, tenantB)
	if n, err := jobs.RebuildAll(ctx); err != nil || n != 1 {
		t.Fatalf("RebuildAll with B suspended = %d, %v", n, err)
	}
	var count int
	if err := public.QueryRow(ctx, `SELECT count(*) FROM status_page_snapshot('page-b')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("suspended page served: %d %v", count, err)
	}

	// A page with no components shows no data, not "operational".
	if _, err := svc.PutComponents(ctx, a, pageA.ID, nil); err != nil {
		t.Fatal(err)
	}
	if s := read("page-a"); s.Status != statuspages.NoData || len(s.Components) != 0 || s.ActiveIncidents == nil {
		t.Fatalf("empty page: %+v", s)
	}
	if got := fmt.Sprint(len(sa.Components[0].Days)); got != "90" {
		t.Fatal(got)
	}
}

// TestIntegrationManyIncidents: 600 flaps of one heartbeat do not push out the open incident of
// another component, the heartbeat's bars and uptime use every flap, and the page lists at most
// MaxPastIncidents past incidents.
func TestIntegrationManyIncidents(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("statuspageflaps"))
	ctx := context.Background()
	f := fixture{t: t, admin: env.AdminConn(t)}
	tenant := uuid.NewString()
	f.exec(`INSERT INTO tenants (id, slug, name) VALUES ($1, 'flaps', 'F')`, tenant)
	f.exec(`INSERT INTO users (id, email) VALUES ($1, 'owner@example.com')`, owner)
	now := time.Now().UTC()
	flappy := f.monitor(tenant, "flappy", "heartbeat", "up", 0, now.AddDate(0, 0, -200))
	broken := f.monitor(tenant, "broken", "http", "down", 2, now.AddDate(0, 0, -200))
	// 600 one-minute incidents spread over the last 60 days, then the open one, which is the oldest.
	f.exec(`INSERT INTO incidents (id, tenant_id, monitor_id, started_at, resolved_at, resolve_reason, reason)
		SELECT gen_random_uuid(), $1, $2, now() - (g * interval '2 hours'), now() - (g * interval '2 hours') + interval '1 minute', 'recovered', 'heartbeat_missed'
		FROM generate_series(1, 600) AS g`, tenant, flappy)
	f.incident(tenant, broken, now.AddDate(0, 0, -70), nil)

	svc := statuspages.NewService(env.Pool(t, env.APIURL, 2), statuspages.Options{})
	a := ownerOf(tenant)
	page, err := svc.Create(ctx, a, statuspages.CreateInput{Slug: "flaps", Title: "Flaps"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutComponents(ctx, a, page.ID, []statuspages.ComponentInput{{MonitorID: flappy, PublicName: "Cron"}, {MonitorID: broken, PublicName: "API"}}); err != nil {
		t.Fatal(err)
	}
	snap, err := svc.Preview(ctx, a, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ActiveIncidents) != 1 || snap.ActiveIncidents[0].Title != "API is unavailable" {
		t.Fatalf("open incident lost: %+v", snap.ActiveIncidents)
	}
	if len(snap.PastIncidents) != statuspages.MaxPastIncidents {
		t.Fatalf("past incidents: %d", len(snap.PastIncidents))
	}
	// 600 minutes down over the 90-day window the monitor existed in (from 89 days ago to now).
	cron := snap.Components[0]
	window := now.Sub(statuspages.HistoryWindow(now)[0])
	want := 100 * (1 - (600*time.Minute).Seconds()/window.Seconds())
	if cron.Uptime == nil || *cron.Uptime > want+0.01 || *cron.Uptime < want-0.01 {
		t.Fatalf("heartbeat uptime %v, want about %.3f (every flap counted)", cron.Uptime, want)
	}
}

// TestIntegrationPauseAndDeleteRefreshThePage: pausing and deleting a monitor change a published
// page in the same transaction, without waiting for jobs.
func TestIntegrationPauseAndDeleteRefreshThePage(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("statuspagepause"))
	ctx := context.Background()
	f := fixture{t: t, admin: env.AdminConn(t)}
	tenant := uuid.NewString()
	f.exec(`INSERT INTO tenants (id, slug, name) VALUES ($1, 'pause', 'P')`, tenant)
	f.exec(`INSERT INTO users (id, email) VALUES ($1, 'owner@example.com')`, owner)
	pool := env.Pool(t, env.APIURL, 2)
	sched := scheduler.New()
	mon := monitors.NewService(pool, sched, incidents.NewService(sched))
	ma := monitors.Actor{TenantID: uuid.MustParse(tenant), UserID: owner, Access: authz.Actor{UserID: owner, Role: authz.Owner}}
	u1, u2 := "https://example.com/1", "https://example.com/2"
	m1, err := mon.Create(ctx, ma, monitors.CreateInput{Type: "http", Name: "one", URL: &u1})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := mon.Create(ctx, ma, monitors.CreateInput{Type: "http", Name: "two", URL: &u2})
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE monitor_state SET status = 'up' WHERE monitor_id = $1`, m1.ID)
	svc := statuspages.NewService(pool, statuspages.Options{})
	a := ownerOf(tenant)
	page, err := svc.Create(ctx, a, statuspages.CreateInput{Slug: "pause-me", Title: "P"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutComponents(ctx, a, page.ID, []statuspages.ComponentInput{{MonitorID: m1.ID, PublicName: "One"}, {MonitorID: m2.ID, PublicName: "Two"}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := svc.Update(ctx, a, page.ID, statuspages.UpdateInput{Published: &yes}); err != nil {
		t.Fatal(err)
	}
	public := env.Conn(t, env.StatuspageURL)
	read := func() statuspages.Snapshot {
		t.Helper()
		var raw []byte
		if err := public.QueryRow(ctx, `SELECT payload FROM status_page_snapshot('pause-me')`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var s statuspages.Snapshot
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := read(); s.Components[0].Status != statuspages.Operational {
		t.Fatalf("before: %+v", s.Components[0])
	}
	if _, err := mon.Pause(ctx, ma, m1.ID); err != nil {
		t.Fatal(err)
	}
	if s := read(); s.Components[0].Status != statuspages.NoData {
		t.Fatalf("after pause: %+v", s.Components[0])
	}
	if err := mon.Delete(ctx, ma, m2.ID); err != nil {
		t.Fatal(err)
	}
	if s := read(); len(s.Components) != 1 || s.Components[0].Name != "One" {
		t.Fatalf("after delete: %+v", s.Components)
	}
}
