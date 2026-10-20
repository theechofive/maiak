package worker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/checks"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
	"github.com/theechofive/maiak/backend/internal/worker"
)

// The worker creates missing partitions itself at start (ADR-0022), so results never depend on
// jobs running.
func TestIntegrationWorkerEnsuresPartitionsAtStart(t *testing.T) {
	e := newEnv(t, "workerpartitions")
	var next, after string
	if err := e.admin.QueryRow(e.ctx, `SELECT 'check_results_p' || to_char(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month', 'YYYYMM'),
		'check_results_p' || to_char(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '2 months', 'YYYYMM')`).Scan(&next, &after); err != nil {
		t.Fatal(err)
	}
	pgtest.MustExec(t, e.admin, `DROP TABLE `+next, `DROP TABLE `+after)
	w, reg := e.worker(&fakeProber{}, worker.Config{ClaimBatch: 10, Concurrency: 5, TenantConcurrency: 5, HostConcurrency: 2, PollInterval: time.Hour})
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM pg_class WHERE relname IN ($1, $2)`, next, after) != 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'check_results'::regclass AND c.relname IN ($1, $2) AND c.relforcerowsecurity`, next, after); n != 2 {
		t.Fatalf("worker did not recreate %s and %s with RLS (%d found)", next, after, n)
	}
	if got := metric(t, reg, "maiak_worker_ensure_partitions_errors_total"); got != 0 {
		t.Fatalf("ensure errors = %v", got)
	}
}

// A result transaction blocked on a lock gives up after 2 s and discards the whole tenant batch
// like a stale lease: nothing written, the lease is left to expire, the metric counts it.
func TestIntegrationLockTimeoutDiscardsTenantBatch(t *testing.T) {
	e := newEnv(t, "workerlocktimeout")
	m := e.monitor(tenantA, "https://locked.example.com/", 1)
	holder := e.AdminConn(t)
	tx, err := holder.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `SELECT 1 FROM monitor_state WHERE monitor_id = $1 FOR UPDATE`, m); err != nil {
		t.Fatal(err)
	}
	w, reg := e.worker(&fakeProber{}, worker.Config{})
	start := time.Now()
	w.RunOnce(e.ctx)
	if took := time.Since(start); took < 2*time.Second || took > 9*time.Second {
		t.Fatalf("RunOnce took %s; want the 2 s lock timeout, not the 10 s flush timeout", took)
	}
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, m); n != 0 {
		t.Fatalf("%d results written from a timed-out batch", n)
	}
	if n := e.count(`SELECT count(*) FROM monitor_location_state WHERE monitor_id = $1 AND lease_token IS NOT NULL`, m); n != 1 {
		t.Fatal("lease of the discarded batch was released; it must expire like a stale one")
	}
	if got := metric(t, reg, "maiak_worker_lock_timeouts_total"); got != 1 {
		t.Fatalf("lock timeouts = %v, want 1", got)
	}
	if got := metric(t, reg, "maiak_worker_write_errors_total"); got != 0 {
		t.Fatalf("write errors = %v, want 0", got)
	}
}

// panicProber panics for URLs containing "panic" (with the URL in the panic value) and answers
// OK otherwise.
type panicProber struct{ fakeProber }

func (p *panicProber) Probe(ctx context.Context, req checks.Request) checks.Result {
	if strings.Contains(req.URL, "panic") {
		panic("probe of " + req.URL + " exploded")
	}
	return p.fakeProber.Probe(ctx, req)
}

// A panicking prober does not crash the worker or leak its concurrency slot, and its URL is not
// logged.
func TestIntegrationProberPanicRecovered(t *testing.T) {
	e := newEnv(t, "workerpanic")
	bad := e.monitor(tenantA, "https://panic.example.com/secret-path", 1)
	good := e.monitor(tenantB, "https://fine.example.com/", 1)
	pgtest.MustExec(t, e.admin, `UPDATE monitor_location_state SET next_run_at = now() - interval '1 minute' WHERE monitor_id = '`+bad.String()+`'`)
	// One slot in total: if the panic leaked it, the second iteration could claim nothing.
	w, reg := e.worker(&panicProber{}, worker.Config{ClaimBatch: 10, Concurrency: 1, TenantConcurrency: 1, HostConcurrency: 1})
	w.RunOnce(e.ctx)
	if got := metric(t, reg, "maiak_worker_prober_panics_total"); got != 1 {
		t.Fatalf("panics = %v, want 1", got)
	}
	w.RunOnce(e.ctx)
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, good); n != 1 {
		t.Fatalf("the slot was not released after the panic: %d results for the next job", n)
	}
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, bad); n != 0 {
		t.Fatalf("panicked check recorded %d results", n)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "prober panicked") || strings.Contains(logs, "panic.example.com") || strings.Contains(logs, "secret-path") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// A permanent ApplyResult error (here: the monitor_state row is gone) keeps the result, completes
// the lease so the job is not repeated at once, and is counted.
func TestIntegrationPermanentApplyErrorCompletesLease(t *testing.T) {
	e := newEnv(t, "workerapplyerror")
	m := e.monitor(tenantA, "https://orphan.example.com/token=abc", 1)
	pgtest.MustExec(t, e.admin, `DELETE FROM monitor_state WHERE monitor_id = '`+m.String()+`'`)
	w, reg := e.worker(&fakeProber{}, worker.Config{})
	w.RunOnce(e.ctx)
	if n := e.count(`SELECT count(*) FROM check_results WHERE monitor_id = $1`, m); n != 1 {
		t.Fatalf("result not kept: %d rows", n)
	}
	if n := e.count(`SELECT count(*) FROM monitor_location_state WHERE monitor_id = $1 AND lease_token IS NULL
		AND next_run_at > now() + interval '50 seconds'`, m); n != 1 {
		t.Fatal("lease not completed with the next run scheduled")
	}
	if got := metric(t, reg, "maiak_worker_apply_errors_total"); got != 1 {
		t.Fatalf("apply errors = %v, want 1", got)
	}
	if got := metric(t, reg, "maiak_worker_write_errors_total"); got != 0 {
		t.Fatalf("write errors = %v, want 0", got)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "monitor_not_found") || strings.Contains(logs, "orphan.example.com") {
		t.Fatalf("logs:\n%s", logs)
	}
}
