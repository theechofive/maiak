package incidents_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/incidents/machine"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/results"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// Concurrent failed results for one monitor (several workers, or a late flush overlapping the
// next one) open exactly one incident: LockMonitorState serialises them and the partial unique
// index is the backstop (ADR-0022).
func TestIntegrationConcurrentApplyResultOpensOneIncident(t *testing.T) {
	f := newFixture(t)
	f.addSlack(tenantA)
	m := f.createMonitor(1, true)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		res := failed("timeout")
		res.CheckedAt = f.clock.Add(time.Duration(i+1) * time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.applyErr(tenantA, m, res); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent ApplyResult: %v", err)
	}
	var open, total int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE resolved_at IS NULL), count(*) FROM incidents WHERE monitor_id = $1`, m).
		Scan(&open, &total); err != nil {
		t.Fatal(err)
	}
	if open != 1 || total != 1 {
		t.Fatalf("incidents: %d open, %d total; want exactly one", open, total)
	}
	if d := f.deliveries(m); len(d) != 1 || d[0].kind != "opened" {
		t.Fatalf("deliveries %+v, want one opened", d)
	}
}

// When the stored state says no incident is open but one is, ApplyResult must not open a second
// one: the unique index rejects it and the error is the permanent ErrInconsistentState.
func TestIntegrationSecondOpenIncidentIsInconsistent(t *testing.T) {
	f := newFixture(t)
	m := f.createMonitor(1, false)
	f.apply(m, failed("timeout"))
	pgtest.MustExec(t, f.admin, `UPDATE monitor_state SET current_incident_id = NULL, status = 'up' WHERE monitor_id = '`+m.String()+`'`,
		`UPDATE monitor_location_state SET status = 'up', consecutive_failures = 0 WHERE monitor_id = '`+m.String()+`'`)
	_, err := f.applyErr(tenantA, m, failed("timeout"))
	if !errors.Is(err, incidents.ErrInconsistentState) {
		t.Fatalf("second open incident: err = %v, want ErrInconsistentState", err)
	}
	var open int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM incidents WHERE monitor_id = $1 AND resolved_at IS NULL`, m).Scan(&open); err != nil || open != 1 {
		t.Fatalf("open incidents = %d (err %v), want 1", open, err)
	}
}

// A pause that arrives while a worker's result transaction is open waits for it (both lock the
// monitor row) and then resolves the incident that transaction opened; nothing stays open.
func TestIntegrationPauseRacingInFlightWrite(t *testing.T) {
	f := newFixture(t)
	f.addSlack(tenantA)
	m := f.createMonitor(1, true)

	applied, commit, written := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		written <- db.WithTenantTx(f.ctx, f.worker, uuid.MustParse(tenantA), func(tx pgx.Tx) error {
			now := time.Now().UTC()
			if err := results.Insert(f.ctx, tx, results.Row{
				TenantID: uuid.MustParse(tenantA), MonitorID: m, LocationID: uuid.MustParse(homeLocation), CheckedAt: now, ErrorKind: "timeout",
			}); err != nil {
				return err
			}
			out, err := f.svc.ApplyResult(f.ctx, tx, incidents.ResultInput{
				TenantID: uuid.MustParse(tenantA), MonitorID: m, LocationID: uuid.MustParse(homeLocation), Now: now,
				Result: machine.Result{ErrorKind: "timeout", CheckedAt: now},
			})
			if err != nil {
				return err
			}
			if out.Transition != machine.Opened {
				return errors.New("the failed result did not open an incident")
			}
			close(applied)
			<-commit
			return nil
		})
	}()
	select {
	case err := <-written:
		t.Fatalf("result transaction ended early: %v", err)
	case <-applied:
	}

	paused := make(chan error, 1)
	go func() { _, err := f.monitors.Pause(f.ctx, f.actor, m); paused <- err }()
	select {
	case err := <-paused:
		t.Fatalf("pause finished while the result transaction held the monitor: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(commit)
	if err := <-written; err != nil {
		t.Fatalf("result transaction: %v", err)
	}
	if err := <-paused; err != nil {
		t.Fatalf("pause: %v", err)
	}

	var open, resolvedPaused int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE resolved_at IS NULL), count(*) FILTER (WHERE resolve_reason = 'paused')
		FROM incidents WHERE monitor_id = $1`, m).Scan(&open, &resolvedPaused); err != nil {
		t.Fatal(err)
	}
	if open != 0 || resolvedPaused != 1 {
		t.Fatalf("after the race: %d open, %d resolved as paused; want 0 and 1", open, resolvedPaused)
	}
	if s := f.state(m); s.current != nil || s.status != "unknown" || s.failures != 0 {
		t.Fatalf("state after pause: %+v", s)
	}
	if d := f.deliveries(m); len(d) != 1 || d[0].kind != "opened" {
		t.Fatalf("deliveries %+v; pause must not notify", d)
	}
	// A result still in flight from before the pause is ignored.
	if out := f.apply(m, machine.Result{OK: true}); !out.Ignored {
		t.Fatal("result after the pause was applied")
	}
}
