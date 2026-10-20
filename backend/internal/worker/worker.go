// Package worker runs the home location's checks (cmd/worker): every second it claims due
// (monitor, location) jobs, runs them on a bounded pool with per-tenant and per-host limits,
// and writes the results in one transaction per tenant batch: result row, lease check,
// incident state machine and outbox (ADR-0016, ADR-0020).
package worker

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/checks"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/incidents/machine"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/results"
	"github.com/theechofive/maiak/backend/internal/scheduler"
)

// Defaults of Config.
const (
	DefaultPollInterval  = time.Second
	DefaultFlushInterval = 500 * time.Millisecond
	DefaultFlushSize     = 50
	DefaultDrainTimeout  = 25 * time.Second
	// MaxJitter spreads next runs by up to this much either way around the interval.
	MaxJitter = time.Second
	// DeferDelay is how long a job given back for lack of capacity waits before it is due again.
	DeferDelay = 2 * time.Second
	// flushTimeout bounds one tenant batch write, also during shutdown.
	flushTimeout = 10 * time.Second
)

// Config tunes the worker. Zero values take the defaults above (limits default to 1).
type Config struct {
	ClaimBatch        int
	Concurrency       int
	TenantConcurrency int
	HostConcurrency   int
	PollInterval      time.Duration
	FlushInterval     time.Duration
	FlushSize         int
	// DrainTimeout is how long shutdown waits for checks in flight before abandoning them
	// (their leases expire and another worker repeats them).
	DrainTimeout time.Duration
}

// Deps are the worker's collaborators.
type Deps struct {
	Pool      *db.Pool
	Scheduler *scheduler.Service
	Incidents *incidents.Service
	Prober    checks.Prober
	Metrics   *Metrics
	Log       *slog.Logger
	// Clock defaults to the system clock; Jitter to a uniform value in [-MaxJitter, MaxJitter].
	Clock  clock.Clock
	Jitter func() time.Duration
}

// Worker claims, runs and records checks.
type Worker struct {
	cfg     Config
	d       Deps
	limiter *Limiter
	batch   *batcher
	running sync.WaitGroup
}

type outcome struct {
	job scheduler.Job
	res checks.Result
}

// New returns a Worker.
func New(cfg Config, d Deps) *Worker {
	cfg.PollInterval = orDefault(cfg.PollInterval, DefaultPollInterval)
	cfg.FlushInterval = orDefault(cfg.FlushInterval, DefaultFlushInterval)
	cfg.DrainTimeout = orDefault(cfg.DrainTimeout, DefaultDrainTimeout)
	cfg.FlushSize = orDefault(cfg.FlushSize, DefaultFlushSize)
	cfg.ClaimBatch = max(cfg.ClaimBatch, 1)
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.Jitter == nil {
		d.Jitter = func() time.Duration {
			return time.Duration(rand.Int64N(int64(2*MaxJitter)+1)) - MaxJitter //nolint:gosec // load spreading
		}
	}
	return &Worker{
		cfg: cfg, d: d,
		limiter: NewLimiter(cfg.Concurrency, cfg.TenantConcurrency, cfg.HostConcurrency),
		batch:   newBatcher(cfg.FlushSize),
	}
}

// Run claims and runs checks until ctx is cancelled, then stops claiming, waits up to
// DrainTimeout for checks in flight, writes everything buffered and returns.
func (w *Worker) Run(ctx context.Context) error {
	// Checks and writes must outlive the shutdown signal: a check cut short by SIGTERM would be
	// recorded as a failure.
	base := context.WithoutCancel(ctx)
	checkCtx, abandon := context.WithCancel(base)
	defer abandon()
	flushCtx, stopFlushing := context.WithCancel(base)
	var flusher sync.WaitGroup
	flusher.Add(1)
	go func() {
		defer flusher.Done()
		w.batch.loop(flushCtx, w.cfg.FlushInterval, func(items []outcome) { w.flush(base, items) })
	}()

	w.d.Log.Info("worker started", "concurrency", w.cfg.Concurrency, "claim_batch", w.cfg.ClaimBatch)
	tick := time.NewTicker(w.cfg.PollInterval)
	defer tick.Stop()
	for {
		w.tick(ctx, checkCtx)
		select {
		case <-ctx.Done():
		case <-tick.C:
			continue
		}
		break
	}

	w.d.Log.Info("worker stopping: waiting for checks in flight", "timeout", w.cfg.DrainTimeout)
	done := make(chan struct{})
	go func() { w.running.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.cfg.DrainTimeout):
		w.d.Log.Warn("abandoning checks in flight; their leases will expire")
		abandon()
		<-done
	}
	stopFlushing()
	flusher.Wait()
	w.flush(base, w.batch.take())
	w.d.Log.Info("worker stopped")
	return nil
}

// RunOnce claims due jobs once, runs them, and writes their results before returning. Tests
// and tools use it; Run is the long-running loop.
func (w *Worker) RunOnce(ctx context.Context) {
	w.tick(ctx, ctx)
	w.running.Wait()
	w.flush(ctx, w.batch.take())
}

// tick claims as many jobs as there are free slots and starts those the limiter admits.
// Jobs of a saturated tenant or host are given back at once (lease released, due again after
// DeferDelay), so they are claimed again on a later tick instead of waiting in this process
// while their lease runs out (ADR-0020).
func (w *Worker) tick(ctx, checkCtx context.Context) {
	free := w.limiter.Free()
	if free <= 0 || ctx.Err() != nil {
		return
	}
	jobs, err := w.d.Scheduler.Claim(ctx, w.d.Pool, min(w.cfg.ClaimBatch, free))
	if err != nil {
		w.d.Metrics.claimErrors.Inc()
		w.d.Log.ErrorContext(ctx, "claim failed", "error", err)
		return
	}
	now := w.d.Clock.Now()
	var deferred []scheduler.Lease
	for _, j := range jobs {
		host := hostOf(j.URL)
		if !w.limiter.TryAcquire(j.TenantID, host) {
			deferred = append(deferred, j.Lease)
			continue
		}
		w.d.Metrics.lag.Observe(max(now.Sub(j.NextRunAt).Seconds(), 0))
		w.running.Add(1)
		go w.check(checkCtx, j, host)
	}
	if len(deferred) > 0 {
		w.d.Metrics.deferred.Add(float64(len(deferred)))
		w.release(ctx, deferred, DeferDelay)
	}
}

func (w *Worker) check(ctx context.Context, j scheduler.Job, host string) {
	defer w.running.Done()
	start := time.Now()
	res := w.d.Prober.Probe(ctx, checks.Request{URL: j.URL, Timeout: j.Timeout, ExpectedStatus: j.ExpectedStatus})
	w.limiter.Release(j.TenantID, host)
	if res.Cancelled || ctx.Err() != nil {
		// Abandoned at shutdown: not a real result (ADR-0018). Give the job back so another
		// worker runs it now instead of after the lease expires.
		w.d.Metrics.cancelled.Inc()
		w.release(context.WithoutCancel(ctx), []scheduler.Lease{j.Lease}, 0)
		return
	}
	w.d.Metrics.duration.Observe(time.Since(start).Seconds())
	label := "ok"
	if !res.OK {
		label = "failed"
	}
	w.d.Metrics.checks.WithLabelValues(label).Inc()
	w.batch.add(outcome{job: j, res: res})
}

// flush writes outcomes in one transaction per tenant. Each outcome has its own savepoint, so
// a stale lease or a broken row is skipped without losing the rest of the batch.
func (w *Worker) flush(ctx context.Context, items []outcome) {
	if len(items) == 0 {
		return
	}
	var order []uuid.UUID
	byTenant := map[uuid.UUID][]outcome{}
	for _, o := range items {
		if _, ok := byTenant[o.job.TenantID]; !ok {
			order = append(order, o.job.TenantID)
		}
		byTenant[o.job.TenantID] = append(byTenant[o.job.TenantID], o)
	}
	for _, tenant := range order {
		w.flushTenant(ctx, tenant, byTenant[tenant])
	}
}

func (w *Worker) flushTenant(ctx context.Context, tenant uuid.UUID, items []outcome) {
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	ctx, _ = logging.WithFields(ctx)
	var stale, failed int
	err := db.WithTenantTx(ctx, w.d.Pool, tenant, func(tx pgx.Tx) error {
		stale, failed = 0, 0
		for _, o := range items {
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			ok, err := w.write(ctx, sp, o)
			switch {
			case err != nil:
				failed++
				w.d.Log.ErrorContext(ctx, "writing result failed", "monitor_id", o.job.MonitorID, "error", err)
				err = sp.Rollback(ctx)
			case !ok:
				stale++
				err = sp.Rollback(ctx)
			default:
				err = sp.Commit(ctx)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		w.d.Metrics.writeErrors.Add(float64(len(items)))
		w.d.Log.ErrorContext(ctx, "result batch failed", "results", len(items), "error", err)
		return
	}
	w.d.Metrics.stale.Add(float64(stale))
	w.d.Metrics.writeErrors.Add(float64(failed))
}

// write records one outcome and reports false when the lease was stale. The result row is
// inserted before the lease check so that this transaction locks the monitor row (foreign key)
// before its schedule row, the same order as pause, resume and delete in the api: the
// opposite order could deadlock with a resume. A stale outcome is rolled back by the caller.
func (w *Worker) write(ctx context.Context, tx pgx.Tx, o outcome) (bool, error) {
	j, r := o.job, o.res
	if err := results.Insert(ctx, tx, results.Row{
		TenantID: j.TenantID, MonitorID: j.MonitorID, LocationID: j.LocationID, CheckedAt: r.CheckedAt,
		OK: r.OK, StatusCode: r.StatusCode, LatencyMs: r.LatencyMs, ErrorKind: r.ErrorKind, TLSNotAfter: r.TLSNotAfter,
	}); err != nil {
		return false, err
	}
	ok, err := w.d.Scheduler.Complete(ctx, tx, j.Lease, r.CheckedAt.Add(j.Interval).Add(w.d.Jitter()))
	if err != nil || !ok {
		return false, err
	}
	_, err = w.d.Incidents.ApplyResult(ctx, tx, incidents.ResultInput{
		TenantID: j.TenantID, MonitorID: j.MonitorID, LocationID: j.LocationID, Now: w.d.Clock.Now(),
		Result: machine.Result{OK: r.OK, ErrorKind: r.ErrorKind, TLSNotAfter: r.TLSNotAfter, CheckedAt: r.CheckedAt},
	})
	return err == nil, err
}

// release gives claimed jobs back unchecked, due again after delay, one transaction per tenant.
func (w *Worker) release(ctx context.Context, leases []scheduler.Lease, delay time.Duration) {
	byTenant := map[uuid.UUID][]scheduler.Lease{}
	for _, l := range leases {
		byTenant[l.TenantID] = append(byTenant[l.TenantID], l)
	}
	for tenant, ls := range byTenant {
		err := db.WithTenantTx(ctx, w.d.Pool, tenant, func(tx pgx.Tx) error {
			for _, l := range ls {
				if err := w.d.Scheduler.Release(ctx, tx, l, delay); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			// The leases expire on their own after 30 s.
			w.d.Log.WarnContext(ctx, "releasing jobs failed", "jobs", len(ls), "error", err)
		}
	}
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// Metrics are the worker's Prometheus metrics, served on the admin port.
type Metrics struct {
	checks      *prometheus.CounterVec
	duration    prometheus.Histogram
	lag         prometheus.Histogram
	stale       prometheus.Counter
	claimErrors prometheus.Counter
	writeErrors prometheus.Counter
	deferred    prometheus.Counter
	cancelled   prometheus.Counter
}

// NewMetrics registers the worker metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		checks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_worker_checks_total", Help: "Checks run, by result (ok or failed).",
		}, []string{"result"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "maiak_worker_check_duration_seconds", Help: "Duration of one check including redirects.",
			Buckets: []float64{0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15},
		}),
		lag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "maiak_worker_scheduler_lag_seconds", Help: "How late a check starts: claim time minus its next_run_at.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}),
		stale: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "maiak_worker_stale_results_total", Help: "Results discarded because their lease had expired or was taken over.",
		}),
		claimErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "maiak_worker_claim_errors_total", Help: "Failed claim_due_monitors calls.",
		}),
		writeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "maiak_worker_write_errors_total", Help: "Results that could not be written.",
		}),
		deferred: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "maiak_worker_deferred_jobs_total", Help: "Claimed jobs given back because their tenant or host was at its limit.",
		}),
		cancelled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "maiak_worker_cancelled_checks_total", Help: "Checks abandoned at shutdown; discarded and given back.",
		}),
	}
	m.checks.WithLabelValues("ok")
	m.checks.WithLabelValues("failed")
	reg.MustRegister(m.checks, m.duration, m.lag, m.stale, m.claimErrors, m.writeErrors, m.deferred, m.cancelled)
	return m
}
