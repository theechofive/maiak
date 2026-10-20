package queries_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const homeLocation = "01920000-0000-7000-8000-000000000001"

var (
	tenantA  = uuid.MustParse("01920000-0000-7000-8000-00000000000a")
	tenantB  = uuid.MustParse("01920000-0000-7000-8000-00000000000b")
	mUp      = uuid.MustParse("01920000-0000-7000-8000-0000000000a1")
	mPaused  = uuid.MustParse("01920000-0000-7000-8000-0000000000a2")
	mNew     = uuid.MustParse("01920000-0000-7000-8000-0000000000a3")
	mDeleted = uuid.MustParse("01920000-0000-7000-8000-0000000000a4")
	mOther   = uuid.MustParse("01920000-0000-7000-8000-0000000000b1")
	incOld   = uuid.MustParse("01920000-0000-7000-8000-0000000000c1")
	incOpen  = uuid.MustParse("01920000-0000-7000-8000-0000000000c2")
)

type fixture struct {
	t     *testing.T
	ctx   context.Context
	admin *pgx.Conn
	svc   *queries.Service
	now   time.Time
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.admin.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) monitor(tenant, id uuid.UUID, created time.Time, paused, deleted bool, status string) {
	f.t.Helper()
	var deletedAt *time.Time
	if deleted {
		deletedAt = &created
	}
	f.exec(`INSERT INTO monitors (id, tenant_id, name, config, paused, created_at, deleted_at)
		VALUES ($1, $2, 'm', '{"url":"https://example.com/","expectedStatus":"204"}', $3, $4, $5)`, id, tenant, paused, created, deletedAt)
	f.exec(`INSERT INTO monitor_locations (tenant_id, monitor_id, location_id) VALUES ($1, $2, $3)`, tenant, id, homeLocation)
	f.exec(`INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at) VALUES ($1, $2, $3, now())`, tenant, id, homeLocation)
	f.exec(`INSERT INTO monitor_state (tenant_id, monitor_id, status, status_changed_at) VALUES ($1, $2, $3, $4)`, tenant, id, status, created)
}

// check inserts a raw result; latency < 0 means no response (a timeout).
func (f *fixture) check(tenant, id uuid.UUID, at time.Time, ok bool, latency int) {
	f.t.Helper()
	var lat, code *int
	var kind *string
	if latency >= 0 {
		c := 200
		lat, code = &latency, &c
	}
	if !ok {
		k := "timeout"
		if latency >= 0 {
			k = "unexpected_status"
		}
		kind = &k
	}
	f.exec(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, status_code, latency_ms, error_kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, tenant, id, homeLocation, at, ok, code, lat, kind)
}

func (f *fixture) rollup(id uuid.UUID, hour time.Time, total, ok int, sum int64, hist []int32) {
	f.t.Helper()
	h := make([]int32, 11)
	copy(h, hist)
	f.exec(`INSERT INTO result_rollups_hourly (tenant_id, monitor_id, location_id, hour, total, ok_count, latency_sum_ms, latency_histogram)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, tenantA, id, homeLocation, hour, total, ok, sum, h)
}

func near(got *float64, want float64) bool {
	return got != nil && math.Abs(*got-want) < 1e-9
}

// The fake "now" is in the third month that ensure_partitions created (current month + 2), so
// every result of the 30-day window has a partition.
func newFixture(t *testing.T) *fixture {
	env := pgtest.New(t, pgtest.DBName("queries"))
	real := time.Now().UTC()
	now := time.Date(real.Year(), real.Month()+2, 1, 12, 7, 0, 0, time.UTC)
	f := &fixture{t: t, ctx: context.Background(), admin: env.AdminConn(t), now: now,
		svc: queries.New(env.Pool(t, env.APIURL, 4), clock.NewFake(now))}
	f.exec(`INSERT INTO tenants (id, slug, name) VALUES ($1, 'a', 'A'), ($2, 'b', 'B')`, tenantA, tenantB)
	created := now.Add(-40 * 24 * time.Hour)
	f.monitor(tenantA, mUp, created, false, false, "down")
	f.monitor(tenantA, mPaused, created.Add(time.Second), true, false, "down")
	f.monitor(tenantA, mNew, created.Add(2*time.Second), false, false, "unknown")
	f.monitor(tenantA, mDeleted, created.Add(3*time.Second), false, true, "up")
	f.monitor(tenantB, mOther, created, false, false, "up")
	return f
}

// seed writes the results of mUp (see the expectations in the tests below):
//
//	head  T-24h+1m          raw: 1 failed without response
//	hour  trunc(T-24h)      rollup 1/0 (the same check, rolled up)
//	hour  H1 = trunc(T-24h)+1h  rollup 60/54, histogram 60 x (100,250], sum 9000; plus 2 raw rows
//	                        (100, 200 ms) that must not be counted twice
//	hour  trunc(T)-1h       raw only (not rolled up yet): 300 ok, 400 ok, 1 timeout
//	hour  trunc(T)          raw only: 50 ok, 123 ok (the newest)
//	T-30h, T-10d            rollups 10/5 (histogram 10 x (5000,10000], sum 50000) and 100/100 (no latency)
func (f *fixture) seed() {
	T := f.now
	h0 := T.Add(-24 * time.Hour).Truncate(time.Hour)
	h1 := h0.Add(time.Hour)
	last, cur := T.Truncate(time.Hour).Add(-time.Hour), T.Truncate(time.Hour)

	f.check(tenantA, mUp, T.Add(-24*time.Hour+time.Minute), false, -1)
	f.rollup(mUp, h0, 1, 0, 0, nil)
	f.rollup(mUp, h1, 60, 54, 9000, []int32{0, 0, 0, 0, 60})
	f.check(tenantA, mUp, h1.Add(time.Minute), true, 100)
	f.check(tenantA, mUp, h1.Add(2*time.Minute), true, 200)
	f.check(tenantA, mUp, last.Add(time.Minute), true, 300)
	f.check(tenantA, mUp, last.Add(2*time.Minute), true, 400)
	f.check(tenantA, mUp, last.Add(20*time.Minute), false, -1)
	f.check(tenantA, mUp, cur.Add(time.Minute), true, 50)
	f.check(tenantA, mUp, cur.Add(5*time.Minute), true, 123)
	f.rollup(mUp, T.Add(-30*time.Hour).Truncate(time.Hour), 10, 5, 50000, []int32{0, 0, 0, 0, 0, 0, 0, 0, 0, 10})
	f.rollup(mUp, T.Add(-10*24*time.Hour).Truncate(time.Hour), 100, 100, 0, nil)
	f.exec(`UPDATE monitor_state SET last_checked_at = $2, last_error_kind = NULL, tls_expires_at = $3 WHERE monitor_id = $1`,
		mUp, cur.Add(5*time.Minute), T.Add(20*24*time.Hour))

	// Incidents of mUp: an old resolved one and the ongoing one.
	f.exec(`INSERT INTO incidents (id, tenant_id, started_at, resolved_at, resolve_reason, reason) VALUES
		($1, $3, $4, $5, 'recovered', 'dns'), ($2, $3, $6, NULL, NULL, 'timeout')`,
		incOld, incOpen, tenantA, T.Add(-5*24*time.Hour), T.Add(-5*24*time.Hour+time.Hour), last.Add(20*time.Minute))
	f.exec(`INSERT INTO incident_monitors (tenant_id, incident_id, monitor_id) VALUES ($1, $2, $4), ($1, $3, $4)`, tenantA, incOld, incOpen, mUp)
	f.exec(`UPDATE monitor_state SET current_incident_id = $2 WHERE monitor_id = $1`, mUp, incOpen)

	// The paused monitor has results; paused or not, its uptime is over checks that ran.
	f.check(tenantA, mPaused, cur.Add(time.Minute), true, 10)
	// The other tenant's monitor.
	f.check(tenantB, mOther, cur.Add(time.Minute), true, 999)
}

func TestIntegrationListAndDetail(t *testing.T) {
	f := newFixture(t)
	f.seed()

	list, sum, err := f.svc.ListMonitors(f.ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != mUp || list[1].ID != mPaused || list[2].ID != mNew {
		t.Fatalf("list = %+v", list)
	}
	if sum != (queries.Summary{Down: 1, Paused: 1, Unknown: 1}) {
		t.Fatalf("summary = %+v", sum)
	}
	up := list[0]
	// 24 h: head 1/0 + rollup H1 60/54 + raw tail 3/2 + 2/2 = 66/58; the rollup of trunc(T-24h)
	// is outside the whole hours of the window, the raw rows of H1 are covered by its rollup.
	if !near(up.Uptime24h, 58.0/66*100) {
		t.Fatalf("uptime24h = %v, want %v", deref(up.Uptime24h), 58.0/66*100)
	}
	if up.LastResponseMs == nil || *up.LastResponseMs != 123 || up.Status != "down" || up.ExpectedStatus != "204" {
		t.Fatalf("mUp = %+v", up)
	}
	if up.Ongoing == nil || up.Ongoing.ID != incOpen || up.Ongoing.Reason != "timeout" {
		t.Fatalf("ongoing = %+v", up.Ongoing)
	}
	paused := list[1]
	if !paused.Paused || paused.Status != "unknown" || !near(paused.Uptime24h, 100) {
		t.Fatalf("paused = %+v", paused)
	}
	if n := list[2]; n.Uptime24h != nil || n.LastResponseMs != nil || n.LastCheckedAt != nil || n.Ongoing != nil {
		t.Fatalf("never checked = %+v", n)
	}

	d, err := f.svc.Monitor(f.ctx, tenantA, mUp)
	if err != nil {
		t.Fatal(err)
	}
	// 7 d: rollups trunc(T-24h) 1/0, H1 60/54, T-30h 10/5, raw after H1 5/4 = 76/63.
	// 30 d: plus the T-10d rollup 100/100 = 176/163.
	if !near(d.Uptime24h, 58.0/66*100) || !near(d.Uptime7d, 63.0/76*100) || !near(d.Uptime30d, 163.0/176*100) {
		t.Fatalf("uptime = %v %v %v", deref(d.Uptime24h), deref(d.Uptime7d), deref(d.Uptime30d))
	}
	if d.TLSExpiresAt == nil || d.Ongoing == nil {
		t.Fatalf("detail = %+v", d)
	}
	nd, err := f.svc.Monitor(f.ctx, tenantA, mNew)
	if err != nil || nd.Uptime24h != nil || nd.Uptime7d != nil || nd.Uptime30d != nil {
		t.Fatalf("new monitor uptime: %+v %v", nd, err)
	}
}

func TestIntegrationTenantIsolationAndNotFound(t *testing.T) {
	f := newFixture(t)
	f.seed()
	list, sum, err := f.svc.ListMonitors(f.ctx, tenantB)
	if err != nil || len(list) != 1 || list[0].ID != mOther || sum != (queries.Summary{Up: 1}) || !near(list[0].Uptime24h, 100) {
		t.Fatalf("tenant B list: %+v %+v %v", list, sum, err)
	}
	for _, id := range []uuid.UUID{mOther, mDeleted, uuid.New()} {
		if _, err := f.svc.Monitor(f.ctx, tenantA, id); !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("monitor %s: %v", id, err)
		}
		if _, err := f.svc.Stats(f.ctx, tenantA, id, queries.Range24h); !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("stats %s: %v", id, err)
		}
		if _, err := f.svc.Checks(f.ctx, tenantA, id, 10); !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("checks %s: %v", id, err)
		}
		if _, err := f.svc.Incidents(f.ctx, tenantA, id); !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("incidents %s: %v", id, err)
		}
	}
}

func TestIntegrationStats(t *testing.T) {
	f := newFixture(t)
	f.seed()

	// 24 h from raw rows: latencies 100, 200, 300, 400, 50, 123 (timeouts have none).
	s, err := f.svc.Stats(f.ctx, tenantA, mUp, queries.Range24h)
	if err != nil {
		t.Fatal(err)
	}
	// percentile_cont(0.95) over [50 100 123 200 300 400]: 300 + 0.75 * 100.
	if !near(s.AvgMs, 1173.0/6) || !near(s.P95Ms, 375) || len(s.Series) != 97 {
		t.Fatalf("24h: avg=%v p95=%v points=%d", deref(s.AvgMs), deref(s.P95Ms), len(s.Series))
	}
	var withData int
	for _, p := range s.Series {
		if p.OKRatio != nil {
			withData++
		}
	}
	// Buckets: head (T-24h+1m), H1 (:01, :02), last hour :01/:02 and :20, current hour :01/:05.
	if withData != 5 {
		t.Fatalf("24h buckets with data = %d, want 5", withData)
	}
	lastBucket := s.Series[len(s.Series)-1]
	if !near(lastBucket.OKRatio, 1) || !near(lastBucket.AvgMs, 86.5) {
		t.Fatalf("current bucket = %v %v", deref(lastBucket.OKRatio), deref(lastBucket.AvgMs))
	}
	if first := s.Series[0]; !near(first.OKRatio, 0) || first.AvgMs != nil {
		t.Fatalf("head bucket = %v %v", deref(first.OKRatio), first.AvgMs)
	}

	// 7 d from rollups and the merged histogram: (100,250]: 60 + 1 (123 ms), (25,50]: 1,
	// (250,500]: 2, (5000,10000]: 10; n = 74, rank 71 falls into the 10000 ms bucket.
	s, err = f.svc.Stats(f.ctx, tenantA, mUp, queries.Range7d)
	if err != nil {
		t.Fatal(err)
	}
	if !near(s.AvgMs, (9000.0+50000+300+400+50+123)/74) || !near(s.P95Ms, 10000) || len(s.Series) != 169 {
		t.Fatalf("7d: avg=%v p95=%v points=%d", deref(s.AvgMs), deref(s.P95Ms), len(s.Series))
	}
	h1 := f.now.Add(-24 * time.Hour).Truncate(time.Hour).Add(time.Hour)
	for _, p := range s.Series {
		if p.T.Equal(h1) && (!near(p.OKRatio, 0.9) || !near(p.AvgMs, 150)) {
			t.Fatalf("7d bucket H1 = %v %v (raw rows counted twice?)", deref(p.OKRatio), deref(p.AvgMs))
		}
	}

	// 30 d: the T-10d rollup adds checks but no latency; p95 unchanged, 6-hour buckets.
	s, err = f.svc.Stats(f.ctx, tenantA, mUp, queries.Range30d)
	if err != nil {
		t.Fatal(err)
	}
	if !near(s.P95Ms, 10000) || len(s.Series) != 121 {
		t.Fatalf("30d: p95=%v points=%d", deref(s.P95Ms), len(s.Series))
	}
	var total int
	for _, p := range s.Series {
		if p.OKRatio != nil {
			total++
		}
	}
	if total < 3 {
		t.Fatalf("30d buckets with data = %d", total)
	}

	// No data at all: nulls, but every bucket is listed.
	s, err = f.svc.Stats(f.ctx, tenantA, mNew, queries.Range7d)
	if err != nil || s.AvgMs != nil || s.P95Ms != nil || len(s.Series) != 169 || s.Series[0].OKRatio != nil {
		t.Fatalf("empty stats: %+v %v", s, err)
	}
}

func TestIntegrationChecksAndIncidents(t *testing.T) {
	f := newFixture(t)
	f.seed()

	all, err := f.svc.Checks(f.ctx, tenantA, mUp, 1000) // clamped to MaxCheckLimit
	if err != nil || len(all) != 8 {
		t.Fatalf("checks: %d %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].CheckedAt.After(all[i-1].CheckedAt) {
			t.Fatal("checks not newest first")
		}
	}
	if c := all[0]; c.ResponseMs == nil || *c.ResponseMs != 123 || !c.OK || c.Location != "home-eu-central-1" || c.ErrorKind != nil {
		t.Fatalf("newest check = %+v", c)
	}
	if c := all[2]; c.OK || c.ResponseMs != nil || c.HTTPStatus != nil || c.ErrorKind == nil || *c.ErrorKind != "timeout" {
		t.Fatalf("timeout check = %+v", c)
	}
	two, err := f.svc.Checks(f.ctx, tenantA, mUp, 2)
	if err != nil || len(two) != 2 || !two[0].CheckedAt.Equal(all[0].CheckedAt) {
		t.Fatalf("limit 2: %+v %v", two, err)
	}
	if none, err := f.svc.Checks(f.ctx, tenantA, mNew, 50); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("no checks: %v %v", none, err)
	}

	inc, err := f.svc.Incidents(f.ctx, tenantA, mUp)
	if err != nil || len(inc) != 2 {
		t.Fatalf("incidents: %+v %v", inc, err)
	}
	if inc[0].ID != incOpen || inc[0].ResolvedAt != nil || inc[0].Reason != "timeout" {
		t.Fatalf("ongoing first: %+v", inc[0])
	}
	if inc[1].ID != incOld || inc[1].ResolvedAt == nil || inc[1].Reason != "dns" {
		t.Fatalf("resolved: %+v", inc[1])
	}

	// At most MaxIncidents.
	for i := 0; i < queries.MaxIncidents+5; i++ {
		id := uuid.New()
		at := f.now.Add(-time.Duration(30+i) * 24 * time.Hour)
		f.exec(`INSERT INTO incidents (id, tenant_id, started_at, resolved_at, resolve_reason, reason) VALUES ($1, $2, $3, $4, 'recovered', 'dns')`,
			id, tenantA, at, at.Add(time.Minute))
		f.exec(`INSERT INTO incident_monitors (tenant_id, incident_id, monitor_id) VALUES ($1, $2, $3)`, tenantA, id, mNew)
	}
	if inc, err := f.svc.Incidents(f.ctx, tenantA, mNew); err != nil || len(inc) != queries.MaxIncidents {
		t.Fatalf("incident limit: %d %v", len(inc), err)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
