package monitors_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const (
	tenantA = "01920000-0000-7000-8000-00000000000a"
	tenantB = "01920000-0000-7000-8000-00000000000b"
)

type hbFixture struct {
	t     *testing.T
	ctx   context.Context
	admin *pgx.Conn
	api   *monitors.Service // as the api runs it (maiak_api)
	jobs  *monitors.Service // as jobs runs it (maiak_worker)
	actor monitors.Actor
}

func newHBFixture(t *testing.T, name string) *hbFixture {
	t.Helper()
	env := pgtest.New(t, pgtest.DBName(name))
	admin := env.AdminConn(t)
	user := uuid.New()
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name, plan_key) VALUES ('`+tenantA+`', 'a', 'A', 'internal'), ('`+tenantB+`', 'b', 'B', 'internal')`,
		`INSERT INTO users (id, email) VALUES ('`+user.String()+`', 'owner@example.com')`)
	sched := scheduler.New()
	return &hbFixture{
		t: t, ctx: context.Background(), admin: admin,
		api:   monitors.NewService(env.Pool(t, env.APIURL, 8), sched, incidents.NewService(sched)),
		jobs:  monitors.NewService(env.Pool(t, env.WorkerURL, 4), sched, incidents.NewService(sched)),
		actor: monitors.Actor{TenantID: uuid.MustParse(tenantA), UserID: user},
	}
}

func (f *hbFixture) create(notify bool) (monitors.Monitor, []byte) {
	f.t.Helper()
	interval, grace := 60, 30
	m, err := f.api.Create(f.ctx, f.actor, monitors.CreateInput{
		Type: monitors.TypeHeartbeat, Name: "Watchdog", NotifySlack: &notify, ExpectedIntervalSeconds: &interval, GraceSeconds: &grace,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if m.HeartbeatToken == "" || m.Status != monitors.StatusUnknown || m.FailureThreshold != 1 || m.IntervalSeconds != 60 || m.GraceSeconds != 30 {
		f.t.Fatalf("created %+v", m)
	}
	return m, monitors.HashHeartbeatToken(m.HeartbeatToken)
}

func (f *hbFixture) ping(hash []byte) monitors.PingResult {
	f.t.Helper()
	res, err := f.api.Ping(f.ctx, hash)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *hbFixture) evaluate() int {
	f.t.Helper()
	n, err := f.jobs.EvaluateHeartbeats(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// age moves the heartbeat's clock back: its wait and last ping happened d ago.
func (f *hbFixture) age(id uuid.UUID, d time.Duration) {
	f.t.Helper()
	if _, err := f.admin.Exec(f.ctx, `UPDATE heartbeat_tokens SET waiting_since = waiting_since - make_interval(secs => $2),
		last_ping_at = last_ping_at - make_interval(secs => $2), applied_ping_at = applied_ping_at - make_interval(secs => $2),
		missed_deadline = missed_deadline - make_interval(secs => $2) WHERE monitor_id = $1`, id, d.Seconds()); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.admin.Exec(f.ctx, `UPDATE monitor_location_state SET last_checked_at = last_checked_at - make_interval(secs => $2) WHERE monitor_id = $1`, id, d.Seconds()); err != nil {
		f.t.Fatal(err)
	}
}

type hbState struct {
	status, lastError string
	incidents         int
	open              *string // reason of the open incident
	resolveReasons    []string
	outbox            []string
}

func (f *hbFixture) state(id uuid.UUID) hbState {
	f.t.Helper()
	var s hbState
	var lastErr *string
	if err := f.admin.QueryRow(f.ctx, `SELECT status, last_error_kind FROM monitor_state WHERE monitor_id = $1`, id).Scan(&s.status, &lastErr); err != nil {
		f.t.Fatal(err)
	}
	if lastErr != nil {
		s.lastError = *lastErr
	}
	rows, err := f.admin.Query(f.ctx, `SELECT reason, resolved_at IS NULL, coalesce(resolve_reason, '') FROM incidents WHERE monitor_id = $1 ORDER BY coalesce(resolved_at, 'infinity'), started_at`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var reason, resolve string
		var open bool
		if err := rows.Scan(&reason, &open, &resolve); err != nil {
			f.t.Fatal(err)
		}
		s.incidents++
		if open {
			s.open = &reason
		} else {
			s.resolveReasons = append(s.resolveReasons, resolve)
		}
	}
	rows, err = f.admin.Query(f.ctx, `SELECT kind || ':' || coalesce(payload ->> 'reason', '') FROM notification_deliveries WHERE monitor_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	s.outbox, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *hbFixture) addSlack(tenant string) {
	f.t.Helper()
	pgtest.MustExec(f.t, f.admin, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted)
		VALUES (gen_random_uuid(), '`+tenant+`', 'slack_webhook', '\x01')`)
}

// TestIntegrationHeartbeatEvaluation drives a heartbeat through ping, missed deadline, recovery,
// pause, resume and delete with the services as api and jobs run them (ADR-0094).
func TestIntegrationHeartbeatEvaluation(t *testing.T) {
	f := newHBFixture(t, "heartbeateval")
	f.addSlack(tenantA)
	m, hash := f.create(true)

	// Never pinged: unknown until the first deadline passes, then down with one incident.
	if n := f.evaluate(); n != 0 {
		t.Fatalf("missed %d before the deadline", n)
	}
	if s := f.state(m.ID); s.status != "unknown" || s.incidents != 0 {
		t.Fatalf("before the deadline: %+v", s)
	}
	f.age(m.ID, 91*time.Second)
	if n := f.evaluate(); n != 1 {
		t.Fatalf("missed %d, want 1", n)
	}
	s := f.state(m.ID)
	if s.status != "down" || s.lastError != monitors.ErrorKindHeartbeatMissed || s.open == nil || *s.open != monitors.ErrorKindHeartbeatMissed ||
		strings.Join(s.outbox, ",") != "opened:heartbeat_missed" {
		t.Fatalf("after the missed deadline: %+v", s)
	}
	// The incident starts at the deadline, not at the evaluation.
	var lag time.Duration
	if err := f.admin.QueryRow(f.ctx, `SELECT i.started_at - (h.waiting_since + interval '90 seconds') FROM incidents i
		JOIN heartbeat_tokens h ON h.monitor_id = i.monitor_id WHERE i.monitor_id = $1`, m.ID).Scan(&lag); err != nil || lag != 0 {
		t.Fatalf("incident start differs from the deadline by %v (%v)", lag, err)
	}
	// The same deadline is applied once, however often jobs runs.
	for range 3 {
		if n := f.evaluate(); n != 0 {
			t.Fatalf("missed again: %d", n)
		}
	}
	if s := f.state(m.ID); s.incidents != 1 || len(s.outbox) != 1 {
		t.Fatalf("after repeated evaluations: %+v", s)
	}

	// A ping resolves it at the next evaluation.
	if res := f.ping(hash); !res.Recorded || res.MonitorID != m.ID || res.TenantID.String() != tenantA {
		t.Fatalf("ping: %+v", res)
	}
	if s := f.state(m.ID); s.status != "down" {
		t.Fatalf("resolved before the evaluation: %+v", s)
	}
	f.evaluate()
	s = f.state(m.ID)
	if s.status != "up" || s.open != nil || strings.Join(s.resolveReasons, ",") != incidents.ReasonRecovered ||
		strings.Join(s.outbox, ",") != "opened:heartbeat_missed,resolved:" {
		t.Fatalf("after the ping: %+v", s)
	}
	// Pings while up change nothing visible.
	f.ping(hash)
	f.evaluate()
	if s := f.state(m.ID); s.status != "up" || s.incidents != 1 || len(s.outbox) != 2 {
		t.Fatalf("ping while up: %+v", s)
	}

	t.Run("pause resolves without a notification, nothing is evaluated while paused", func(t *testing.T) {
		f.age(m.ID, 2*time.Minute)
		if n := f.evaluate(); n != 1 {
			t.Fatalf("second miss: %d", n)
		}
		if _, err := f.api.Pause(f.ctx, f.actor, m.ID); err != nil {
			t.Fatal(err)
		}
		s := f.state(m.ID)
		if s.status != "unknown" || s.open != nil || strings.Join(s.resolveReasons, ",") != "recovered,paused" || len(s.outbox) != 3 {
			t.Fatalf("after pause: %+v", s)
		}
		if res := f.ping(hash); res.Recorded {
			t.Fatal("ping recorded while paused")
		}
		f.age(m.ID, time.Hour)
		if n := f.evaluate(); n != 0 {
			t.Fatalf("missed while paused: %d", n)
		}
		// Resume restarts the wait: unknown until a ping or the next deadline from now.
		if _, err := f.api.Resume(f.ctx, f.actor, m.ID); err != nil {
			t.Fatal(err)
		}
		if n := f.evaluate(); n != 0 {
			t.Fatalf("missed right after resume: %d", n)
		}
		if s := f.state(m.ID); s.status != "unknown" || s.open != nil {
			t.Fatalf("after resume: %+v", s)
		}
		f.ping(hash)
		f.evaluate()
		if s := f.state(m.ID); s.status != "up" {
			t.Fatalf("ping after resume: %+v", s)
		}
	})

	t.Run("delete resolves and stops evaluation", func(t *testing.T) {
		f.age(m.ID, 2*time.Minute)
		f.evaluate()
		if s := f.state(m.ID); s.open == nil {
			t.Fatalf("not down before delete: %+v", s)
		}
		if err := f.api.Delete(f.ctx, f.actor, m.ID); err != nil {
			t.Fatal(err)
		}
		if res := f.ping(hash); res.Recorded {
			t.Fatal("ping recorded after delete")
		}
		f.age(m.ID, time.Hour)
		if n := f.evaluate(); n != 0 {
			t.Fatalf("deleted heartbeat evaluated: %d", n)
		}
		s := f.state(m.ID)
		if s.open != nil || s.resolveReasons[len(s.resolveReasons)-1] != incidents.ReasonDeleted {
			t.Fatalf("after delete: %+v", s)
		}
	})
}

// The outbox gets a row only when the heartbeat notifies Slack and the tenant has a channel.
func TestIntegrationHeartbeatOutboxRules(t *testing.T) {
	f := newHBFixture(t, "heartbeatoutbox")
	quiet, _ := f.create(false)
	noisy, _ := f.create(true)
	f.age(quiet.ID, 2*time.Minute)
	f.age(noisy.ID, 2*time.Minute)
	if n := f.evaluate(); n != 2 {
		t.Fatalf("missed %d, want 2", n)
	}
	for _, id := range []uuid.UUID{quiet.ID, noisy.ID} {
		if s := f.state(id); s.status != "down" || s.open == nil || len(s.outbox) != 0 {
			t.Fatalf("no channel: %+v", s)
		}
	}
	f.addSlack(tenantA)
	other, _ := f.create(true)
	silent, _ := f.create(false)
	f.age(other.ID, 2*time.Minute)
	f.age(silent.ID, 2*time.Minute)
	f.evaluate()
	if s := f.state(other.ID); strings.Join(s.outbox, ",") != "opened:heartbeat_missed" {
		t.Fatalf("notify on, channel: %+v", s)
	}
	if s := f.state(silent.ID); s.open == nil || len(s.outbox) != 0 {
		t.Fatalf("notify off, channel: %+v", s)
	}
	var payload []byte
	if err := f.admin.QueryRow(f.ctx, `SELECT payload FROM notification_deliveries WHERE monitor_id = $1`, other.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil || p["url"] != "" || p["monitorName"] != "Watchdog" || p["startedAt"] == nil {
		t.Fatalf("payload %s (%v)", payload, err)
	}
}

// A heartbeat's token only ever touches its own tenant's monitor, and quotas are per type.
func TestIntegrationHeartbeatTenantsAndQuota(t *testing.T) {
	f := newHBFixture(t, "heartbeattenants")
	a, hashA := f.create(true)
	f.actor.TenantID = uuid.MustParse(tenantB)
	b, hashB := f.create(true)
	if res := f.ping(hashA); res.TenantID.String() != tenantA || res.MonitorID != a.ID {
		t.Fatalf("A: %+v", res)
	}
	var pingedB *time.Time
	if err := f.admin.QueryRow(f.ctx, `SELECT last_ping_at FROM heartbeat_tokens WHERE monitor_id = $1`, b.ID).Scan(&pingedB); err != nil || pingedB != nil {
		t.Fatalf("B touched by A's token: %v %v", pingedB, err)
	}
	if res := f.ping(hashB); res.TenantID.String() != tenantB || res.MonitorID != b.ID {
		t.Fatalf("B: %+v", res)
	}
	// Tenant A cannot see, pause or regenerate B's heartbeat.
	f.actor.TenantID = uuid.MustParse(tenantA)
	if _, err := f.api.RegenerateHeartbeatToken(f.ctx, f.actor, b.ID); !errors.Is(err, monitors.ErrNotFound) {
		t.Fatalf("regenerate across tenants: %v", err)
	}
	if _, err := f.api.Pause(f.ctx, f.actor, b.ID); !errors.Is(err, monitors.ErrNotFound) {
		t.Fatalf("pause across tenants: %v", err)
	}
	if _, err := f.api.Ping(f.ctx, monitors.HashHeartbeatToken("unknown")); !errors.Is(err, monitors.ErrUnknownHeartbeat) {
		t.Fatalf("unknown token: %v", err)
	}

	// The free plan: one heartbeat, separate from the three monitors; concurrent creates cannot
	// overshoot it.
	pgtest.MustExec(t, f.admin, `UPDATE tenants SET plan_key = 'free' WHERE id = '`+tenantB+`'`)
	f.actor.TenantID = uuid.MustParse(tenantB)
	if _, err := f.api.Create(f.ctx, f.actor, monitors.CreateInput{Type: monitors.TypeHeartbeat, Name: "second"}); !isLimit(err, plans.ResourceHeartbeat, 1) {
		t.Fatalf("second heartbeat on free: %v", err)
	}
	if err := f.api.Delete(f.ctx, f.actor, b.ID); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 5)
	for range 5 {
		go func() {
			_, err := f.api.Create(f.ctx, f.actor, monitors.CreateInput{Type: monitors.TypeHeartbeat, Name: "racer"})
			errs <- err
		}()
	}
	ok := 0
	for range 5 {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case !isLimit(err, plans.ResourceHeartbeat, 1):
			t.Fatalf("racer: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d concurrent creates succeeded at the free limit of 1", ok)
	}
	url := "https://example.com/"
	for i := range 3 {
		if _, err := f.api.Create(f.ctx, f.actor, monitors.CreateInput{Name: "site", URL: &url}); err != nil {
			t.Fatalf("http monitor %d next to a full heartbeat quota: %v", i, err)
		}
	}
	if _, err := f.api.Create(f.ctx, f.actor, monitors.CreateInput{Name: "site", URL: &url}); !isLimit(err, plans.ResourceMonitor, 3) {
		t.Fatalf("fourth http monitor on free: %v", err)
	}
}

func isLimit(err error, r plans.Resource, limit int) bool {
	var le *plans.LimitError
	return errors.As(err, &le) && le.Resource == r && le.Max == limit
}

// A ping of a heartbeat that is already up is only marked as seen; the incident machine runs for
// pings that change the state (unknown or down to up), as before (ADR-0096).
func TestIntegrationHeartbeatPingWhileUp(t *testing.T) {
	f := newHBFixture(t, "heartbeatpingup")
	m, hash := f.create(false)
	times := func() (lastPing, applied, stateChecked, locChecked time.Time) {
		t.Helper()
		if err := f.admin.QueryRow(f.ctx, `SELECT h.last_ping_at, h.applied_ping_at, s.last_checked_at, l.last_checked_at
			FROM heartbeat_tokens h JOIN monitor_state s ON s.monitor_id = h.monitor_id
			JOIN monitor_location_state l ON l.monitor_id = h.monitor_id WHERE h.monitor_id = $1`, m.ID).
			Scan(&lastPing, &applied, &stateChecked, &locChecked); err != nil {
			t.Fatal(err)
		}
		return
	}

	// unknown -> up: the machine applies the ping.
	f.ping(hash)
	f.evaluate()
	first, applied, stateChecked, locChecked := times()
	if s := f.state(m.ID); s.status != "up" || !applied.Equal(first) || !stateChecked.Equal(first) || !locChecked.Equal(first) {
		t.Fatalf("first ping: %+v applied %v checked %v/%v ping %v", s, applied, stateChecked, locChecked, first)
	}

	// up -> up: only applied_ping_at moves.
	f.ping(hash)
	f.evaluate()
	second, applied, stateChecked, locChecked := times()
	if !second.After(first) || !applied.Equal(second) || !stateChecked.Equal(first) || !locChecked.Equal(first) {
		t.Fatalf("ping while up: ping %v applied %v checked %v/%v, want the machine untouched at %v", second, applied, stateChecked, locChecked, first)
	}
	if s := f.state(m.ID); s.status != "up" || s.incidents != 0 {
		t.Fatalf("ping while up: %+v", s)
	}
	if n := f.evaluate(); n != 0 {
		t.Fatalf("re-evaluated: %d", n)
	}

	// down -> up: the machine applies the ping again and resolves the incident at the ping time.
	f.age(m.ID, 2*time.Minute)
	if n := f.evaluate(); n != 1 {
		t.Fatalf("miss: %d", n)
	}
	f.ping(hash)
	f.evaluate()
	third, applied, stateChecked, locChecked := times()
	s := f.state(m.ID)
	if s.status != "up" || s.open != nil || s.incidents != 1 || !applied.Equal(third) || !stateChecked.Equal(third) || !locChecked.Equal(third) {
		t.Fatalf("recovery: %+v ping %v applied %v checked %v/%v", s, third, applied, stateChecked, locChecked)
	}
}
