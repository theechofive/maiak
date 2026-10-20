package worker_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/checks"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
	"github.com/theechofive/maiak/backend/internal/worker"
)

const (
	tenantA      = "01920000-0000-7000-8000-00000000000a"
	tenantB      = "01920000-0000-7000-8000-00000000000b"
	homeLocation = "01920000-0000-7000-8000-000000000001"
)

type env struct {
	*pgtest.Env
	t     *testing.T
	ctx   context.Context
	admin *pgx.Conn
	logs  syncBuffer
}

// syncBuffer collects the workers' logs so tests can check what they leave out.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newEnv(t *testing.T, name string) *env {
	e := &env{Env: pgtest.New(t, pgtest.DBName(name)), t: t, ctx: context.Background()}
	e.admin = e.AdminConn(t)
	pgtest.MustExec(t, e.admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	return e
}

// monitor creates a monitor due now (url is stored as is, bypassing the api's URL policy).
func (e *env) monitor(tenant, url string, threshold int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	pgtest.MustExec(e.t, e.admin,
		fmt.Sprintf(`INSERT INTO monitors (id, tenant_id, name, config, failure_threshold) VALUES ('%s', '%s', 'm', jsonb_build_object('url', '%s', 'expectedStatus', '2xx'), %d)`,
			id, tenant, url, threshold),
		fmt.Sprintf(`INSERT INTO monitor_locations (tenant_id, monitor_id, location_id) VALUES ('%s', '%s', '%s')`, tenant, id, homeLocation),
		fmt.Sprintf(`INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at) VALUES ('%s', '%s', '%s', now())`, tenant, id, homeLocation),
		fmt.Sprintf(`INSERT INTO monitor_state (tenant_id, monitor_id) VALUES ('%s', '%s')`, tenant, id),
	)
	return id
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRow(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e *env) worker(p checks.Prober, cfg worker.Config) (*worker.Worker, *prometheus.Registry) {
	reg := prometheus.NewRegistry()
	sched := scheduler.New()
	if cfg.Concurrency == 0 {
		cfg = worker.Config{ClaimBatch: 100, Concurrency: 50, TenantConcurrency: 5, HostConcurrency: 2,
			PollInterval: 50 * time.Millisecond, FlushInterval: 20 * time.Millisecond}
	}
	return worker.New(cfg, worker.Deps{
		Pool: e.Pool(e.t, e.WorkerURL, 4), Scheduler: sched, Incidents: incidents.NewService(sched),
		Prober: p, Metrics: worker.NewMetrics(reg), Log: slog.New(slog.NewTextHandler(&e.logs, nil)),
		Jitter: func() time.Duration { return 0 },
	}), reg
}

func metric(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			switch {
			case m.Counter != nil:
				sum += m.GetCounter().GetValue()
			case m.Histogram != nil:
				sum += float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return sum
}

// fakeProber answers OK and counts calls per URL; hook runs inside Probe when set.
type fakeProber struct {
	mu    sync.Mutex
	calls map[string]int
	hook  func()
}

func (f *fakeProber) Probe(_ context.Context, req checks.Request) checks.Result {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[req.URL]++
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return checks.Result{OK: true, StatusCode: 200, LatencyMs: 12, CheckedAt: time.Now().UTC()}
}

func TestIntegrationTwoWorkersCheckOncePerInterval(t *testing.T) {
	e := newEnv(t, "workertwo")
	const n = 12
	var ids []uuid.UUID
	for i := range n {
		tenant := tenantA
		if i%2 == 1 {
			tenant = tenantB
		}
		ids = append(ids, e.monitor(tenant, fmt.Sprintf("https://site%d.example.com/", i), 2))
	}
	p := &fakeProber{}
	w1, reg1 := e.worker(p, worker.Config{})
	w2, reg2 := e.worker(p, worker.Config{})
	ctx, cancel := context.WithCancel(e.ctx)
	var wg sync.WaitGroup
	for _, w := range []*worker.Worker{w1, w2} {
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.Run(ctx) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM check_results`) < n && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	// Keep both workers polling a while longer: nothing is due again for 60 s.
	time.Sleep(500 * time.Millisecond)
	cancel()
	wg.Wait()

	for _, id := range ids {
		if got := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, id); got != 1 {
			t.Errorf("monitor %s checked %d times, want 1", id, got)
		}
	}
	for url, c := range p.calls {
		if c != 1 {
			t.Errorf("%s probed %d times", url, c)
		}
	}
	if bad := e.count(`SELECT count(*) FROM monitor_location_state s JOIN check_results r USING (monitor_id)
		WHERE s.lease_token IS NOT NULL OR s.next_run_at <> r.checked_at + interval '60 seconds' OR s.status <> 'up'`); bad != 0 {
		t.Errorf("%d schedules not rescheduled to checked_at + 60 s with the lease released", bad)
	}
	if total := metric(t, reg1, "maiak_worker_checks_total") + metric(t, reg2, "maiak_worker_checks_total"); total != n {
		t.Errorf("checks_total across workers = %v, want %d", total, n)
	}
	if lag := metric(t, reg1, "maiak_worker_scheduler_lag_seconds") + metric(t, reg2, "maiak_worker_scheduler_lag_seconds"); lag != n {
		t.Errorf("scheduler lag observations = %v, want %d", lag, n)
	}
}

func TestIntegrationCrashedWorkerLeaseExpires(t *testing.T) {
	e := newEnv(t, "workercrash")
	m := e.monitor(tenantA, "https://crash.example.com/", 2)
	// A worker claims the job and dies without writing anything.
	jobs, err := scheduler.New().Claim(e.ctx, e.Pool(t, e.WorkerURL, 1), 10, scheduler.HomeLocationCode)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v %v", jobs, err)
	}
	p := &fakeProber{}
	w, _ := e.worker(p, worker.Config{})
	w.RunOnce(e.ctx)
	if n := e.count(`SELECT count(*) FROM check_results`); n != 0 || len(p.calls) != 0 {
		t.Fatalf("job with a live lease was checked (%d results, %d probes)", n, len(p.calls))
	}
	// After the 30 s lease (simulated) another worker takes over.
	pgtest.MustExec(t, e.admin, `UPDATE monitor_location_state SET lease_until = now() - interval '1 second'`)
	w.RunOnce(e.ctx)
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, m); n != 1 {
		t.Fatalf("expired lease not reclaimed: %d results", n)
	}
	if n := e.count(`SELECT count(*) FROM monitor_location_state WHERE lease_token IS NULL AND last_checked_at IS NOT NULL`); n != 1 {
		t.Fatal("lease not released after the write")
	}
	// The crashed worker's late write is fenced off by its old token.
	if n := e.count(`SELECT count(*) FROM monitor_location_state WHERE lease_token = $1`, jobs[0].Token); n != 0 {
		t.Fatal("old token still holds the row")
	}
}

func TestIntegrationResultAfterLeaseExpiryDiscarded(t *testing.T) {
	e := newEnv(t, "workerstale")
	m := e.monitor(tenantA, "https://slow.example.com/", 1)
	// The check outlives its lease: the hook expires it while the probe runs.
	p := &fakeProber{hook: func() {
		pgtest.MustExec(t, e.admin, `UPDATE monitor_location_state SET lease_until = now() - interval '1 second'`)
	}}
	w, reg := e.worker(p, worker.Config{})
	w.RunOnce(e.ctx)
	if p.calls["https://slow.example.com/"] != 1 {
		t.Fatal("check did not run")
	}
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, m); n != 0 {
		t.Fatalf("stale result written (%d rows)", n)
	}
	if n := e.count(`SELECT count(*) FROM monitor_state WHERE monitor_id = $1 AND status = 'unknown' AND last_checked_at IS NULL`, m); n != 1 {
		t.Fatal("stale result changed monitor state")
	}
	if got := metric(t, reg, "maiak_worker_stale_results_total"); got != 1 {
		t.Fatalf("stale results metric = %v, want 1", got)
	}
}

func TestIntegrationTenantLimitDefersWithoutBlocking(t *testing.T) {
	e := newEnv(t, "workerlimit")
	for i := range 4 {
		e.monitor(tenantA, fmt.Sprintf("https://a%d.example.com/", i), 2)
	}
	b := e.monitor(tenantB, "https://b.example.com/", 2)
	release := make(chan struct{})
	p := &fakeProber{hook: func() { <-release }}
	w, reg := e.worker(p, worker.Config{ClaimBatch: 100, Concurrency: 10, TenantConcurrency: 2, HostConcurrency: 2,
		FlushInterval: 20 * time.Millisecond, PollInterval: time.Hour})
	// Database clock readings around the claim bound the deferred next_run_at exactly, however
	// slow the test machine is (the earlier fixed 1-3 s window was timing-sensitive, L-093).
	dbNow := func() time.Time {
		var ts time.Time
		if err := e.admin.QueryRow(e.ctx, `SELECT clock_timestamp()`).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	before := dbNow()
	done := make(chan struct{})
	go func() { w.RunOnce(e.ctx); close(done) }()
	// Tenant A gets 2 checks, tenant B is not held up, 2 jobs of A go back at once.
	inFlight := func() (int, int) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.calls), p.calls["https://b.example.com/"]
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := inFlight(); n == 3 && e.count(`SELECT count(*) FROM monitor_location_state WHERE lease_token IS NULL`) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	running, bRunning := inFlight()
	if running != 3 || bRunning != 1 {
		t.Fatalf("in flight: %v; want 2 of tenant A and the one of tenant B", p.calls)
	}
	if got := metric(t, reg, "maiak_worker_deferred_jobs_total"); got != 2 {
		t.Fatalf("deferred = %v, want 2", got)
	}
	after := dbNow()
	if n := e.count(`SELECT count(*) FROM monitor_location_state WHERE lease_token IS NULL
		AND next_run_at BETWEEN $1::timestamptz + make_interval(secs => $3) AND $2::timestamptz + make_interval(secs => $3)`,
		before, after, worker.DeferDelay.Seconds()); n != 2 {
		t.Fatalf("deferred jobs not released with the defer delay: %d", n)
	}
	close(release)
	<-done
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, b); n != 1 {
		t.Fatalf("tenant B result missing")
	}
	// The deferred jobs run once they are due again.
	pgtest.MustExec(t, e.admin, `UPDATE monitor_location_state SET next_run_at = now() WHERE lease_token IS NULL AND last_checked_at IS NULL`)
	w.RunOnce(e.ctx)
	if n := e.count(`SELECT count(*) FROM check_results`); n != 5 {
		t.Fatalf("results after second iteration = %d, want 5", n)
	}
}

// TestIntegrationWorkerIterationAgainstTLSServer runs real HTTPS checks against a loopback TLS
// server. The production client from safehttp refuses loopback by design, so the prober uses
// the test server's client here: this covers the worker, the prober, the result write, the
// state machine and the outbox, not the guard. The guard is covered end to end by
// internal/e2e (make test-e2e).
func TestIntegrationWorkerIterationAgainstTLSServer(t *testing.T) {
	e := newEnv(t, "workere2e")
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer ts.Close()
	m := e.monitor(tenantA, ts.URL+"/health", 2)
	pgtest.MustExec(t, e.admin, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted)
		VALUES (gen_random_uuid(), '`+tenantA+`', 'slack_webhook', '\x01')`)
	w, _ := e.worker(checks.NewHTTPSProber(ts.Client()), worker.Config{})
	due := func() { pgtest.MustExec(t, e.admin, `UPDATE monitor_location_state SET next_run_at = now()`) }

	w.RunOnce(e.ctx)
	var ok bool
	var code, latency *int
	var tlsNotAfter *time.Time
	if err := e.admin.QueryRow(e.ctx, `SELECT ok, status_code, latency_ms, tls_not_after FROM check_results WHERE monitor_id = $1`, m).
		Scan(&ok, &code, &latency, &tlsNotAfter); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(ts.TLS.Certificates[0].Certificate[0])
	if !ok || code == nil || *code != 204 || latency == nil || tlsNotAfter == nil || !tlsNotAfter.Equal(cert.NotAfter) {
		t.Fatalf("result ok=%t code=%v latency=%v tls=%v", ok, code, latency, tlsNotAfter)
	}
	var msStatus string
	var lastChecked *time.Time
	if err := e.admin.QueryRow(e.ctx, `SELECT status, last_checked_at FROM monitor_state WHERE monitor_id = $1`, m).Scan(&msStatus, &lastChecked); err != nil ||
		msStatus != "up" || lastChecked == nil {
		t.Fatalf("monitor state %s %v %v", msStatus, lastChecked, err)
	}

	status.Store(http.StatusInternalServerError)
	for range 2 {
		due()
		w.RunOnce(e.ctx)
	}
	var kinds []string
	rows, _ := e.admin.Query(e.ctx, `SELECT kind FROM notification_deliveries WHERE monitor_id = $1 ORDER BY created_at, id`, m)
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || fmt.Sprint(kinds) != "[opened]" {
		t.Fatalf("deliveries %v %v", kinds, err)
	}
	if n := e.count(`SELECT count(*) FROM incidents WHERE resolved_at IS NULL AND reason = 'unexpected_status'`); n != 1 {
		t.Fatal("no open incident")
	}

	status.Store(http.StatusOK)
	due()
	w.RunOnce(e.ctx)
	if n := e.count(`SELECT count(*) FROM incidents WHERE resolve_reason = 'recovered'`); n != 1 {
		t.Fatal("incident not resolved on recovery")
	}
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, m); n != 4 {
		t.Fatalf("results = %d, want 4", n)
	}
}

// ctxProber blocks until release is closed or its context ends.
type ctxProber struct {
	started chan struct{}
	release chan struct{}
}

func (p *ctxProber) Probe(ctx context.Context, _ checks.Request) checks.Result {
	close(p.started)
	select {
	case <-p.release:
		return checks.Result{OK: true, StatusCode: 200, LatencyMs: 5, CheckedAt: time.Now().UTC()}
	case <-ctx.Done():
		return checks.Result{Cancelled: true, CheckedAt: time.Now().UTC()}
	}
}

// cancelledProber reports every check as cancelled, as the real prober does when its parent
// context is cancelled, without the worker's own context being cancelled.
type cancelledProber struct{}

func (cancelledProber) Probe(context.Context, checks.Request) checks.Result {
	return checks.Result{Cancelled: true, CheckedAt: time.Now().UTC()}
}

// A cancelled result is discarded: no result row, no state change, and the lease is released
// so the job is due again at once (ADR-0018).
func TestIntegrationCancelledResultDiscarded(t *testing.T) {
	e := newEnv(t, "workercancelled")
	m := e.monitor(tenantA, "https://cancelled.example.com/", 1)
	w, reg := e.worker(cancelledProber{}, worker.Config{ClaimBatch: 10, Concurrency: 5, TenantConcurrency: 5, HostConcurrency: 2})
	w.RunOnce(e.ctx)
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, m); n != 0 {
		t.Fatalf("cancelled check was recorded (%d rows)", n)
	}
	if n := e.count(`SELECT count(*) FROM monitor_location_state WHERE monitor_id = $1 AND lease_token IS NULL
		AND status = 'unknown' AND last_checked_at IS NULL AND next_run_at <= now()`, m); n != 1 {
		t.Fatal("lease not released or state changed")
	}
	if got := metric(t, reg, "maiak_worker_cancelled_checks_total"); got != 1 {
		t.Fatalf("cancelled metric = %v", got)
	}
}

func TestIntegrationGracefulShutdown(t *testing.T) {
	cfg := worker.Config{ClaimBatch: 10, Concurrency: 5, TenantConcurrency: 5, HostConcurrency: 2,
		PollInterval: 50 * time.Millisecond, FlushInterval: time.Hour}

	t.Run("a check in flight finishes and is written", func(t *testing.T) {
		e := newEnv(t, "workerdrain")
		m := e.monitor(tenantA, "https://drain.example.com/", 2)
		p := &ctxProber{started: make(chan struct{}), release: make(chan struct{})}
		cfg.DrainTimeout = 10 * time.Second
		w, _ := e.worker(p, cfg)
		ctx, cancel := context.WithCancel(e.ctx)
		errc := make(chan error, 1)
		go func() { errc <- w.Run(ctx) }()
		<-p.started
		cancel() // SIGTERM while the check runs
		time.Sleep(100 * time.Millisecond)
		close(p.release)
		if err := <-errc; err != nil {
			t.Fatalf("Run returned %v", err)
		}
		// The flush interval is an hour: only the final flush can have written it.
		if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1 AND ok`, m); n != 1 {
			t.Fatalf("in-flight result not written at shutdown (%d rows)", n)
		}
	})

	t.Run("a check that outlives the drain timeout is abandoned, not recorded as failed", func(t *testing.T) {
		e := newEnv(t, "workerabandon")
		m := e.monitor(tenantA, "https://hang.example.com/", 1)
		p := &ctxProber{started: make(chan struct{}), release: make(chan struct{})}
		cfg.DrainTimeout = 100 * time.Millisecond
		w, _ := e.worker(p, cfg)
		ctx, cancel := context.WithCancel(e.ctx)
		errc := make(chan error, 1)
		go func() { errc <- w.Run(ctx) }()
		<-p.started
		cancel()
		if err := <-errc; err != nil {
			t.Fatalf("Run returned %v", err)
		}
		if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, m); n != 0 {
			t.Fatalf("abandoned check was recorded (%d rows)", n)
		}
		if n := e.count(`SELECT count(*) FROM incidents`); n != 0 {
			t.Fatal("abandoned check opened an incident")
		}
		if n := e.count(`SELECT count(*) FROM monitor_location_state WHERE monitor_id = $1 AND lease_token IS NULL`, m); n != 1 {
			t.Fatal("abandoned check kept its lease")
		}
	})
}
