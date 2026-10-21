package incidents_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/incidents/machine"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const (
	tenantA      = "01920000-0000-7000-8000-00000000000a"
	tenantB      = "01920000-0000-7000-8000-00000000000b"
	homeLocation = "01920000-0000-7000-8000-000000000001"
)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	admin    *pgx.Conn
	worker   *db.Pool
	svc      *incidents.Service
	monitors *monitors.Service
	actor    monitors.Actor
	clock    time.Time
}

func newFixture(t *testing.T) *fixture {
	env := pgtest.New(t, pgtest.DBName("incidents"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	user := uuid.New()
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`,
		`INSERT INTO users (id, email) VALUES ('`+user.String()+`', 'owner@example.com')`)
	sched := scheduler.New()
	svc := incidents.NewService(sched)
	return &fixture{
		t: t, ctx: ctx, admin: admin, worker: env.Pool(t, env.WorkerURL, 4), svc: svc,
		monitors: monitors.NewService(env.Pool(t, env.APIURL, 4), sched, svc),
		actor:    monitors.Actor{TenantID: uuid.MustParse(tenantA), UserID: user},
		clock:    time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fixture) createMonitor(threshold int, notify bool) uuid.UUID {
	f.t.Helper()
	url := "https://shop.example.com/health?token=s3cr3t"
	m, err := f.monitors.Create(f.ctx, f.actor, monitors.CreateInput{
		Name: "Shop", URL: &url, FailureThreshold: &threshold, NotifySlack: &notify,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return m.ID
}

// apply feeds one result through the service as the worker does, one minute after the last.
func (f *fixture) apply(monitor uuid.UUID, res machine.Result) machine.Output {
	f.t.Helper()
	out, err := f.applyErr(tenantA, monitor, res)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) applyErr(tenant string, monitor uuid.UUID, res machine.Result) (machine.Output, error) {
	if res.CheckedAt.IsZero() {
		f.clock = f.clock.Add(time.Minute)
		res.CheckedAt = f.clock
	}
	var out machine.Output
	err := db.WithTenantTx(f.ctx, f.worker, uuid.MustParse(tenant), func(tx pgx.Tx) error {
		var err error
		out, err = f.svc.ApplyResult(f.ctx, tx, incidents.ResultInput{
			TenantID: uuid.MustParse(tenant), MonitorID: monitor, LocationID: uuid.MustParse(homeLocation), Result: res, Now: res.CheckedAt,
		})
		return err
	})
	return out, err
}

type state struct {
	status, locStatus string
	failures          int
	lastError         *string
	current           *uuid.UUID
	warned            *time.Time
}

func (f *fixture) state(monitor uuid.UUID) state {
	f.t.Helper()
	var s state
	if err := f.admin.QueryRow(f.ctx, `SELECT ms.status, ls.status, ls.consecutive_failures, ms.last_error_kind, ms.current_incident_id, ms.tls_warned_not_after
		FROM monitor_state ms JOIN monitor_location_state ls ON ls.monitor_id = ms.monitor_id WHERE ms.monitor_id = $1`, monitor).
		Scan(&s.status, &s.locStatus, &s.failures, &s.lastError, &s.current, &s.warned); err != nil {
		f.t.Fatal(err)
	}
	return s
}

type delivery struct {
	kind     string
	incident *uuid.UUID
	payload  map[string]any
}

func (f *fixture) deliveries(monitor uuid.UUID) []delivery {
	f.t.Helper()
	rows, _ := f.admin.Query(f.ctx, `SELECT kind, incident_id, payload FROM notification_deliveries WHERE monitor_id = $1 ORDER BY created_at, id`, monitor)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (delivery, error) {
		var d delivery
		var raw []byte
		if err := r.Scan(&d.kind, &d.incident, &raw); err != nil {
			return d, err
		}
		return d, json.Unmarshal(raw, &d.payload)
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) incident(id uuid.UUID) (resolved bool, reason *string, cause *string) {
	f.t.Helper()
	if err := f.admin.QueryRow(f.ctx, `SELECT resolved_at IS NOT NULL, resolve_reason, reason FROM incidents i
		JOIN incident_monitors im ON im.incident_id = i.id WHERE i.id = $1`, id).Scan(&resolved, &reason, &cause); err != nil {
		f.t.Fatal(err)
	}
	return resolved, reason, cause
}

func (f *fixture) addSlack(tenant string) {
	f.t.Helper()
	pgtest.MustExec(f.t, f.admin, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted)
		VALUES (gen_random_uuid(), '`+tenant+`', 'slack_webhook', '\x01')`)
}

func failed(kind string) machine.Result { return machine.Result{ErrorKind: kind} }

func TestIntegrationIncidentLifecycle(t *testing.T) {
	f := newFixture(t)
	m := f.createMonitor(2, true)

	// No Slack channel yet: the incident opens but nothing is queued.
	f.apply(m, machine.Result{OK: true})
	if out := f.apply(m, failed("timeout")); out.Transition != machine.None {
		t.Fatalf("first failure: %s", out.Transition)
	}
	if out := f.apply(m, failed("connect")); out.Transition != machine.Opened || len(out.Emit) != 1 {
		t.Fatalf("second failure: %s %v", out.Transition, out.Emit)
	}
	s := f.state(m)
	if s.status != "down" || s.locStatus != "down" || s.failures != 2 || s.current == nil || s.lastError == nil || *s.lastError != "connect" {
		t.Fatalf("after opening: %+v", s)
	}
	if resolved, _, cause := f.incident(*s.current); resolved || cause == nil || *cause != "connect" {
		t.Fatalf("incident resolved=%t cause=%v", resolved, cause)
	}
	if d := f.deliveries(m); len(d) != 0 {
		t.Fatalf("deliveries without a channel: %v", d)
	}

	// With a channel: recovery resolves and queues "resolved" with a payload free of secrets.
	f.addSlack(tenantA)
	opened := *s.current
	if out := f.apply(m, machine.Result{OK: true}); out.Transition != machine.Resolved {
		t.Fatalf("recovery: %s", out.Transition)
	}
	s = f.state(m)
	if s.status != "up" || s.current != nil || s.lastError != nil || s.failures != 0 {
		t.Fatalf("after recovery: %+v", s)
	}
	if resolved, reason, _ := f.incident(opened); !resolved || reason == nil || *reason != incidents.ReasonRecovered {
		t.Fatalf("incident resolved=%t reason=%v", resolved, reason)
	}
	d := f.deliveries(m)
	if len(d) != 1 || d[0].kind != "resolved" || d[0].incident == nil || *d[0].incident != opened ||
		d[0].payload["url"] != "https://shop.example.com/health" || d[0].payload["monitorName"] != "Shop" ||
		d[0].payload["durationSeconds"] != 60.0 || d[0].payload["startedAt"] != "2026-10-20T12:03:00Z" {
		t.Fatalf("resolved delivery: %+v", d)
	}
	raw, _ := json.Marshal(d[0].payload)
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatalf("payload leaks the query string: %s", raw)
	}

	// Down again: "opened" is queued in the same transaction as the incident.
	f.apply(m, failed("timeout"))
	f.apply(m, failed("timeout"))
	d = f.deliveries(m)
	if len(d) != 2 || d[1].kind != "opened" || d[1].payload["reason"] != "timeout" || d[1].incident == nil || *d[1].incident != *f.state(m).current {
		t.Fatalf("opened delivery: %+v", d)
	}

	// A result older than the last one changes nothing.
	if out := f.apply(m, machine.Result{OK: true, CheckedAt: f.clock.Add(-time.Hour)}); !out.Ignored || f.state(m).status != "down" {
		t.Fatal("out-of-order result was applied")
	}

	// TLS warning once per certificate.
	notAfter := f.clock.Add(10 * 24 * time.Hour)
	for range 2 {
		f.apply(m, machine.Result{OK: true, TLSNotAfter: &notAfter})
	}
	d = f.deliveries(m)
	kinds := []string{}
	for _, x := range d {
		kinds = append(kinds, x.kind)
	}
	if fmt.Sprint(kinds) != "[resolved opened resolved tls_expiring]" || d[3].payload["daysLeft"] == nil || d[3].incident != nil {
		t.Fatalf("deliveries %v, last %+v", kinds, d[len(d)-1])
	}
	if w := f.state(m).warned; w == nil || !w.Equal(notAfter) {
		t.Fatalf("tls_warned_not_after = %v", w)
	}

	// Another tenant's transaction cannot see the monitor.
	if _, err := f.applyErr(tenantB, m, failed("dns")); !errors.Is(err, incidents.ErrMonitorNotFound) {
		t.Fatalf("other tenant: err = %v, want ErrMonitorNotFound", err)
	}
}

func TestIntegrationNotifyOff(t *testing.T) {
	f := newFixture(t)
	f.addSlack(tenantA)
	m := f.createMonitor(1, false)
	if out := f.apply(m, failed("dns")); out.Transition != machine.Opened || len(out.Emit) != 0 {
		t.Fatalf("notify off: %s %v", out.Transition, out.Emit)
	}
	f.apply(m, machine.Result{OK: true})
	if d := f.deliveries(m); len(d) != 0 {
		t.Fatalf("deliveries with notifySlack off: %v", d)
	}
}

func TestIntegrationPauseAndDeleteResolveIncidents(t *testing.T) {
	f := newFixture(t)
	f.addSlack(tenantA)
	for _, c := range []struct {
		reason string
		action func(id uuid.UUID) error
	}{
		{incidents.ReasonPaused, func(id uuid.UUID) error { _, err := f.monitors.Pause(f.ctx, f.actor, id); return err }},
		{incidents.ReasonDeleted, func(id uuid.UUID) error { return f.monitors.Delete(f.ctx, f.actor, id) }},
	} {
		t.Run(c.reason, func(t *testing.T) {
			m := f.createMonitor(1, true)
			f.apply(m, failed("timeout"))
			open := f.state(m).current
			if open == nil {
				t.Fatal("no incident opened")
			}
			if err := c.action(m); err != nil {
				t.Fatal(err)
			}
			if resolved, reason, _ := f.incident(*open); !resolved || reason == nil || *reason != c.reason {
				t.Fatalf("incident resolved=%t reason=%v, want %s", resolved, reason, c.reason)
			}
			if s := f.state(m); s.current != nil {
				t.Fatalf("current incident still set: %+v", s)
			}
			// Only the "opened" row: pause and delete never notify.
			if d := f.deliveries(m); len(d) != 1 || d[0].kind != "opened" {
				t.Fatalf("deliveries %+v", d)
			}
			// A late result of a paused or deleted monitor is ignored.
			if out := f.apply(m, machine.Result{OK: true}); !out.Ignored {
				t.Fatal("result for a paused/deleted monitor was applied")
			}
		})
	}

	t.Run("pause without an open incident", func(t *testing.T) {
		m := f.createMonitor(2, true)
		if _, err := f.monitors.Pause(f.ctx, f.actor, m); err != nil {
			t.Fatal(err)
		}
	})
}
