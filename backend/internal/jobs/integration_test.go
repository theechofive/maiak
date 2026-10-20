package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/jobs"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const (
	tenantA      = "01920000-0000-7000-8000-00000000000a"
	tenantB      = "01920000-0000-7000-8000-00000000000b"
	userID       = "01920000-0000-7000-8000-0000000000ee"
	homeLocation = "01920000-0000-7000-8000-000000000001"
	webhookA     = "https://hooks.slack.com/services/TAAA/BAAA/secretAAAA"
	webhookB     = "https://hooks.slack.com/services/TBBB/BBBB/secretBBBB"
)

// fakeSlack is the Slack side: it answers with the queued statuses (200 when the queue is empty)
// and records every message with the webhook path it was posted to.
type fakeSlack struct {
	mu         sync.Mutex
	statuses   []int
	retryAfter string
	posts      []post
	srv        *httptest.Server
}

type post struct{ path, text string }

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.posts = append(f.posts, post{r.URL.Path, body.Text})
		status := http.StatusOK
		if len(f.statuses) > 0 {
			status, f.statuses = f.statuses[0], f.statuses[1:]
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) queue(statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, statuses...)
}

func (f *fakeSlack) sent() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts)
}

// Do sends the notifier's request to the fake server (the production guard refuses loopback).
func (f *fakeSlack) Do(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(f.srv.URL)
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = u.Scheme, u.Host, ""
	return f.srv.Client().Do(out)
}

type env struct {
	*pgtest.Env
	t     *testing.T
	ctx   context.Context
	admin *pgx.Conn
	keys  *crypto.Keyring
	slack *fakeSlack
	// channel ids by tenant
	channels map[string]uuid.UUID
	logs     *bytes.Buffer
	logMu    sync.Mutex
}

func newEnv(t *testing.T, name string) *env {
	keys, err := crypto.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte{3}, 32)}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{Env: pgtest.New(t, pgtest.DBName(name)), t: t, ctx: context.Background(), keys: keys, slack: newFakeSlack(t),
		channels: map[string]uuid.UUID{}, logs: &bytes.Buffer{}}
	e.admin = e.AdminConn(t)
	pgtest.MustExec(t, e.admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`,
		`INSERT INTO users (id, email) VALUES ('`+userID+`', 'u@example.com')`)
	// Channels are configured through the owning service, so the stored value is the real envelope.
	settings := notify.NewService(e.Pool(t, e.APIURL, 2), keys, nil, "")
	for tenant, hook := range map[string]string{tenantA: webhookA, tenantB: webhookB} {
		if _, err := settings.Put(e.ctx, notify.Actor{TenantID: uuid.MustParse(tenant), UserID: uuid.MustParse(userID)}, hook); err != nil {
			t.Fatal(err)
		}
		var id uuid.UUID
		if err := e.admin.QueryRow(e.ctx, `SELECT id FROM notification_channels WHERE tenant_id = $1`, tenant).Scan(&id); err != nil {
			t.Fatal(err)
		}
		e.channels[tenant] = id
	}
	return e
}

func (e *env) Write(p []byte) (int, error) {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return e.logs.Write(p)
}

func (e *env) monitor(tenant, name string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	pgtest.MustExec(e.t, e.admin,
		fmt.Sprintf(`INSERT INTO monitors (id, tenant_id, name, config) VALUES ('%s', '%s', '%s', '{"url":"https://example.com/"}')`, id, tenant, name))
	return id
}

// deliver queues an outbox row like the incident service does and returns its id.
func (e *env) deliver(tenant string, monitor uuid.UUID, kind string, payload map[string]any) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	var incident *uuid.UUID
	if kind != notify.DeliveryTLSExpiring {
		inc := uuid.New()
		incident = &inc
		pgtest.MustExec(e.t, e.admin, fmt.Sprintf(`INSERT INTO incidents (id, tenant_id, started_at) VALUES ('%s', '%s', now())`, inc, tenant))
	}
	payload["monitorId"] = monitor
	raw, _ := json.Marshal(payload)
	if _, err := e.admin.Exec(e.ctx, `INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, incident_id, channel_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, tenant, kind, monitor, incident, e.channels[tenant], raw); err != nil {
		e.t.Fatal(err)
	}
	return id
}

type row struct {
	status    string
	attempts  int
	lastError *string
	retryIn   time.Duration
}

func (e *env) row(id uuid.UUID) row {
	e.t.Helper()
	var r row
	if err := e.admin.QueryRow(e.ctx, `SELECT status, attempts, last_error, next_attempt_at - now() FROM notification_deliveries WHERE id = $1`, id).
		Scan(&r.status, &r.attempts, &r.lastError, &r.retryIn); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) makeDue() {
	pgtest.MustExec(e.t, e.admin, `UPDATE notification_deliveries SET next_attempt_at = now() WHERE status = 'pending'`)
}

// deliverer runs as maiak_worker. Its rate-limit waits are recorded in sleeps instead of slept.
func (e *env) deliverer(reg prometheus.Registerer, sleeps *[]time.Duration) *jobs.Deliverer {
	return e.delivererWithClock(reg, sleeps, nil)
}

func (e *env) delivererWithClock(reg prometheus.Registerer, sleeps *[]time.Duration, clk clock.Clock) *jobs.Deliverer {
	var mu sync.Mutex
	return jobs.NewDeliverer(jobs.DeliveryConfig{PublicURL: "https://maiak.example.com", Concurrency: 10}, jobs.DeliveryDeps{
		Pool:     e.Pool(e.t, e.WorkerURL, 4),
		Channels: notify.NewService(e.Pool(e.t, e.WorkerURL, 2), e.keys, nil, ""),
		Notifier: notify.NewSlackNotifier(e.slack),
		Metrics:  jobs.NewMetrics(reg),
		Log:      slog.New(slog.NewJSONHandler(e, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Clock:    clk,
		Sleep: func(_ context.Context, d time.Duration) error {
			if sleeps != nil {
				mu.Lock()
				*sleeps = append(*sleeps, d)
				mu.Unlock()
			}
			return nil
		},
	})
}

// metric returns the value of the series of name whose label values are exactly values (any order).
func metric(t *testing.T, reg *prometheus.Registry, name string, values ...string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			match := len(m.GetLabel()) == len(values)
			for _, l := range m.GetLabel() {
				match = match && slices.Contains(values, l.GetValue())
			}
			if !match {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func TestIntegrationDelivery(t *testing.T) {
	e := newEnv(t, "jobsdelivery")
	reg := prometheus.NewRegistry()
	var sleeps []time.Duration
	d := e.deliverer(reg, &sleeps)
	started := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	t.Run("success", func(t *testing.T) {
		m := e.monitor(tenantA, "Web")
		id := e.deliver(tenantA, m, notify.DeliveryOpened, map[string]any{"monitorName": "Web", "url": "https://web.example/", "reason": "timeout", "startedAt": started})
		if n := d.RunOnce(e.ctx); n != 1 {
			t.Fatalf("claimed %d", n)
		}
		if r := e.row(id); r.status != "sent" || r.attempts != 1 || r.lastError != nil {
			t.Fatalf("row = %+v", r)
		}
		p := e.slack.sent()
		if len(p) != 1 || p[0].path != "/services/TAAA/BAAA/secretAAAA" || !strings.HasPrefix(p[0].text, ":red_circle: *Web* is down\nhttps://web.example/") ||
			!strings.Contains(p[0].text, "<https://maiak.example.com/monitors/"+m.String()+"|Open in Maiak>") {
			t.Fatalf("posts = %+v", p)
		}
		if got := metric(t, reg, "maiak_jobs_deliveries_total", "sent"); got != 1 {
			t.Fatalf("sent metric = %v", got)
		}
	})

	t.Run("retry then success", func(t *testing.T) {
		// The deliverer's clock is an hour ahead of the database: the retry time is exactly
		// clock + backoff and far from the database's now, so neither check depends on how fast
		// the test runs (the earlier window of 0-3 s from the database clock was timing-sensitive,
		// L-093).
		var dbNow time.Time
		if err := e.admin.QueryRow(e.ctx, `SELECT now()`).Scan(&dbNow); err != nil {
			t.Fatal(err)
		}
		clk := clock.NewFake(dbNow.Add(time.Hour))
		d := e.delivererWithClock(prometheus.NewRegistry(), nil, clk)
		m := e.monitor(tenantA, "Api")
		id := e.deliver(tenantA, m, notify.DeliveryResolved, map[string]any{"monitorName": "Api", "durationSeconds": 120})
		e.slack.queue(500)
		d.RunOnce(e.ctx)
		r := e.row(id)
		var retryAt time.Time
		if err := e.admin.QueryRow(e.ctx, `SELECT next_attempt_at FROM notification_deliveries WHERE id = $1`, id).Scan(&retryAt); err != nil {
			t.Fatal(err)
		}
		if r.status != "pending" || r.attempts != 1 || r.lastError == nil || *r.lastError != "http_status 500" || !retryAt.Equal(clk.Now().Add(jobs.Backoff(1))) {
			t.Fatalf("after 500: %+v, retry at %s, want %s", r, retryAt, clk.Now().Add(jobs.Backoff(1)))
		}
		if n := d.RunOnce(e.ctx); n != 0 {
			t.Fatalf("claimed %d before the retry time", n)
		}
		e.makeDue()
		d.RunOnce(e.ctx)
		if r := e.row(id); r.status != "sent" || r.attempts != 2 {
			t.Fatalf("after retry: %+v", r)
		}
	})

	t.Run("429 honours Retry-After", func(t *testing.T) {
		m := e.monitor(tenantA, "Rl")
		id := e.deliver(tenantA, m, notify.DeliveryResolved, map[string]any{"monitorName": "Rl", "durationSeconds": 5})
		e.slack.retryAfter = "45"
		e.slack.queue(429)
		d.RunOnce(e.ctx)
		r := e.row(id)
		if r.status != "pending" || r.retryIn < 40*time.Second || r.retryIn > 50*time.Second || *r.lastError != "rate_limited" {
			t.Fatalf("after 429: %+v", r)
		}
		pgtest.MustExec(t, e.admin, `UPDATE notification_deliveries SET status = 'failed' WHERE id = '`+id.String()+`'`)
	})

	t.Run("five failures mark failed", func(t *testing.T) {
		m := e.monitor(tenantB, "Flaky")
		id := e.deliver(tenantB, m, notify.DeliveryResolved, map[string]any{"monitorName": "Flaky", "durationSeconds": 5})
		e.slack.queue(500, 502, 503, 500, 500)
		for i := 0; i < jobs.MaxAttempts; i++ {
			e.makeDue()
			d.RunOnce(e.ctx)
		}
		if r := e.row(id); r.status != "failed" || r.attempts != 5 {
			t.Fatalf("after five failures: %+v", r)
		}
		e.makeDue()
		if n := d.RunOnce(e.ctx); n != 0 {
			t.Fatal("a failed delivery was claimed again")
		}
	})

	t.Run("ordering per monitor", func(t *testing.T) {
		m := e.monitor(tenantB, "Ord")
		opened := e.deliver(tenantB, m, notify.DeliveryOpened, map[string]any{"monitorName": "Ord", "startedAt": started})
		resolved := e.deliver(tenantB, m, notify.DeliveryResolved, map[string]any{"monitorName": "Ord", "durationSeconds": 60})
		before := len(e.slack.sent())
		e.slack.queue(500)
		d.RunOnce(e.ctx)
		// opened failed and waits for its retry; resolved must not overtake it.
		e.makeDue()
		pgtest.MustExec(t, e.admin, `UPDATE notification_deliveries SET next_attempt_at = now() + interval '1 hour' WHERE id = '`+opened.String()+`'`)
		if n := d.RunOnce(e.ctx); n != 0 {
			t.Fatalf("resolved overtook opened (%d claimed)", n)
		}
		e.makeDue()
		d.RunOnce(e.ctx) // opened only: resolved was not eligible while opened was in flight
		d.RunOnce(e.ctx) // resolved
		p := e.slack.sent()[before:]
		if len(p) != 3 || !strings.Contains(p[0].text, "is down") || !strings.Contains(p[1].text, "is down") || !strings.Contains(p[2].text, "back up") {
			t.Fatalf("order = %+v", p)
		}
		if e.row(opened).status != "sent" || e.row(resolved).status != "sent" {
			t.Fatal("not both sent")
		}
	})

	t.Run("per-channel rate limit", func(t *testing.T) {
		sleeps = nil
		for i := range 3 {
			e.deliver(tenantA, e.monitor(tenantA, fmt.Sprintf("r%d", i)), notify.DeliveryResolved, map[string]any{"monitorName": "r", "durationSeconds": 1})
		}
		if n := d.RunOnce(e.ctx); n != 3 {
			t.Fatalf("claimed %d", n)
		}
		slices.Sort(sleeps)
		if len(sleeps) < 2 || sleeps[len(sleeps)-1] < 1500*time.Millisecond {
			t.Fatalf("three messages to one channel waited %v; want about 1 s and 2 s", sleeps)
		}
	})

	t.Run("revoked webhook fails the delivery and the channel; later rows fail without sending", func(t *testing.T) {
		m1, m2 := e.monitor(tenantB, "G1"), e.monitor(tenantB, "G2")
		first := e.deliver(tenantB, m1, notify.DeliveryResolved, map[string]any{"monitorName": "G1", "durationSeconds": 1})
		e.slack.queue(404)
		d.RunOnce(e.ctx)
		if r := e.row(first); r.status != "failed" || r.attempts != 1 || *r.lastError != "webhook_not_found" {
			t.Fatalf("after 404: %+v", r)
		}
		var status string
		if err := e.admin.QueryRow(e.ctx, `SELECT status FROM notification_channels WHERE tenant_id = $1`, tenantB).Scan(&status); err != nil || status != "failed" {
			t.Fatalf("channel status %q %v", status, err)
		}
		before := len(e.slack.sent())
		second := e.deliver(tenantB, m2, notify.DeliveryResolved, map[string]any{"monitorName": "G2", "durationSeconds": 1})
		d.RunOnce(e.ctx)
		if r := e.row(second); r.status != "failed" || *r.lastError != "channel_failed" || len(e.slack.sent()) != before {
			t.Fatalf("delivery to a failed channel: %+v, posts %d -> %d", r, before, len(e.slack.sent()))
		}
		// Tenant A is unaffected.
		if err := e.admin.QueryRow(e.ctx, `SELECT status FROM notification_channels WHERE tenant_id = $1`, tenantA).Scan(&status); err != nil || status != "active" {
			t.Fatalf("tenant A channel %q", status)
		}
	})

	t.Run("outbox gauge and no secrets in logs", func(t *testing.T) {
		e.deliver(tenantA, e.monitor(tenantA, "P"), notify.DeliveryResolved, map[string]any{"monitorName": "P", "durationSeconds": 1})
		ctx, cancel := context.WithTimeout(e.ctx, 300*time.Millisecond)
		defer cancel()
		_ = d.Run(ctx) // refreshes the gauge at start, then delivers the row
		if got := metric(t, reg, "maiak_jobs_outbox_pending"); got != 1 {
			t.Fatalf("outbox_pending = %v", got)
		}
		e.logMu.Lock()
		logs := e.logs.String()
		e.logMu.Unlock()
		if !strings.Contains(logs, "webhook_not_found") || strings.Contains(logs, "secret") || strings.Contains(logs, "hooks.slack.com") || strings.Contains(logs, "127.0.0.1") {
			t.Fatalf("logs:\n%s", logs)
		}
	})
}

// A delivery whose jobs process dies (lease expiry) every time ends as failed after
// jobs.MaxAttempts claims: claim_deliveries counts the requeue as an attempt, with the same
// maximum as the Go side. A finish with the claim token of an expired claim is rejected.
func TestIntegrationPoisonDeliveryFailsAfterMaxAttempts(t *testing.T) {
	e := newEnv(t, "jobspoison")
	id := e.deliver(tenantA, e.monitor(tenantA, "Poison"), notify.DeliveryResolved, map[string]any{"monitorName": "Poison", "durationSeconds": 1})
	worker := e.Conn(t, e.WorkerURL)
	claim := func() (string, bool) {
		var token string
		err := worker.QueryRow(e.ctx, `SELECT claim_token::text FROM claim_deliveries(10) WHERE id = $1`, id).Scan(&token)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		return token, true
	}
	var tokens []string
	for i := range jobs.MaxAttempts {
		token, ok := claim()
		if !ok {
			t.Fatalf("claim %d: row not claimed", i+1)
		}
		tokens = append(tokens, token)
		pgtest.MustExec(t, e.admin, `UPDATE notification_deliveries SET lease_until = now() - interval '1 second' WHERE id = '`+id.String()+`'`)
	}
	if _, ok := claim(); ok {
		t.Fatal("poison row claimed after MaxAttempts expired leases")
	}
	if r := e.row(id); r.status != "failed" || r.attempts != jobs.MaxAttempts || r.lastError == nil || *r.lastError != "lease_expired" {
		t.Fatalf("poison row = %+v", r)
	}
	var recorded bool
	if err := worker.QueryRow(e.ctx, `SELECT finish_delivery($1, $2, 'sent', NULL, NULL)`, id, tokens[len(tokens)-1]).Scan(&recorded); err != nil || recorded {
		t.Fatalf("finish after requeue = %t (err %v), want false", recorded, err)
	}
}

// Two jobs processes claiming concurrently never deliver one row twice.
func TestIntegrationConcurrentDeliverersClaimOnce(t *testing.T) {
	e := newEnv(t, "jobsconcurrent")
	var ids []uuid.UUID
	for i := range 30 {
		tenant := tenantA
		if i%2 == 1 {
			tenant = tenantB
		}
		ids = append(ids, e.deliver(tenant, e.monitor(tenant, fmt.Sprintf("c%d", i)), notify.DeliveryResolved, map[string]any{"monitorName": "c", "durationSeconds": 1}))
	}
	d1, d2 := e.deliverer(prometheus.NewRegistry(), nil), e.deliverer(prometheus.NewRegistry(), nil)
	var claimed atomic.Int64
	var wg sync.WaitGroup
	for _, d := range []*jobs.Deliverer{d1, d2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				claimed.Add(int64(d.RunOnce(e.ctx)))
			}
		}()
	}
	wg.Wait()
	if claimed.Load() != 30 || len(e.slack.sent()) != 30 {
		t.Fatalf("claimed %d, posted %d; want 30 each", claimed.Load(), len(e.slack.sent()))
	}
	for _, id := range ids {
		if r := e.row(id); r.status != "sent" || r.attempts != 1 {
			t.Fatalf("%s: %+v", id, r)
		}
	}
}

func TestIntegrationHousekeeping(t *testing.T) {
	e := newEnv(t, "jobshousekeeping")
	reg := prometheus.NewRegistry()
	m := jobs.NewMetrics(reg)
	log := slog.New(slog.NewJSONHandler(e, nil))
	tasks := jobs.DefaultTasks(m, log)
	h := jobs.NewHousekeeper(e.Pool(t, e.WorkerURL, 2), tasks, m, log, nil)
	task := func(name string) jobs.Task {
		for _, t := range tasks {
			if t.Name == name {
				return t
			}
		}
		t.Fatalf("no task %s", name)
		return jobs.Task{}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := e.admin.QueryRow(e.ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	t.Run("advisory lock: a second replica skips the task", func(t *testing.T) {
		pgtest.MustExec(t, e.admin,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ('`+tenantA+`', '`+userID+`', 'owner')`,
			`INSERT INTO sessions (id, user_id, tenant_id, token_hash, csrf_token, expires_at) VALUES
				(gen_random_uuid(), '`+userID+`', '`+tenantA+`', sha256('live'), 'c', now() + interval '1 hour'),
				(gen_random_uuid(), '`+userID+`', '`+tenantA+`', sha256('dead'), 'c', now() - interval '1 second')`)
		purge := task("purge_expired_sessions")
		holder := e.Conn(t, e.WorkerURL)
		if _, err := holder.Exec(e.ctx, `SELECT pg_advisory_lock($1)`, purge.LockKey); err != nil {
			t.Fatal(err)
		}
		if got := h.RunTask(e.ctx, purge); got != "skipped" {
			t.Fatalf("with the lock held elsewhere: %s", got)
		}
		if n := count(`SELECT count(*) FROM sessions`); n != 2 {
			t.Fatalf("skipped run purged sessions: %d left", n)
		}
		if _, err := holder.Exec(e.ctx, `SELECT pg_advisory_unlock($1)`, purge.LockKey); err != nil {
			t.Fatal(err)
		}
		if got := h.RunTask(e.ctx, purge); got != "ok" {
			t.Fatalf("after unlock: %s", got)
		}
		if n := count(`SELECT count(*) FROM sessions WHERE token_hash = sha256('live')`); n != 1 || count(`SELECT count(*) FROM sessions`) != 1 {
			t.Fatal("purge did not delete exactly the expired session")
		}
		if got := metric(t, reg, "maiak_jobs_housekeeping_runs_total", "purge_expired_sessions", "skipped"); got != 1 {
			t.Fatalf("skipped metric = %v", got)
		}
	})

	t.Run("ensure_partitions recreates a missing future partition", func(t *testing.T) {
		var next string
		if err := e.admin.QueryRow(e.ctx, `SELECT 'check_results_p' || to_char(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '2 months', 'YYYYMM')`).Scan(&next); err != nil {
			t.Fatal(err)
		}
		pgtest.MustExec(t, e.admin, `DROP TABLE `+pgx.Identifier{next}.Sanitize())
		if got := h.RunTask(e.ctx, task("ensure_partitions")); got != "ok" {
			t.Fatal(got)
		}
		if count(`SELECT count(*) FROM pg_class WHERE relname = $1`, next) != 1 {
			t.Fatalf("%s not recreated", next)
		}
	})

	t.Run("compute_rollups catches up the closed hours since the earliest raw hour", func(t *testing.T) {
		mon := e.monitor(tenantA, "R")
		pgtest.MustExec(t, e.admin,
			`INSERT INTO monitor_locations (tenant_id, monitor_id, location_id) VALUES ('`+tenantA+`', '`+mon.String()+`', '`+homeLocation+`')`,
			`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, latency_ms, error_kind) VALUES
				('`+tenantA+`', '`+mon.String()+`', '`+homeLocation+`', date_trunc('hour', now(), 'UTC') - interval '90 minutes', true, 40, NULL),
				('`+tenantA+`', '`+mon.String()+`', '`+homeLocation+`', date_trunc('hour', now(), 'UTC') - interval '80 minutes', false, NULL, 'timeout'),
				('`+tenantA+`', '`+mon.String()+`', '`+homeLocation+`', date_trunc('hour', now(), 'UTC') - interval '5 hours', true, 40, NULL),
				('`+tenantA+`', '`+mon.String()+`', '`+homeLocation+`', date_trunc('hour', now(), 'UTC') + interval '1 second', true, 40, NULL)`)
		for range 2 { // idempotent
			if got := h.RunTask(e.ctx, task("compute_rollups")); got != "ok" {
				t.Fatal(got)
			}
		}
		// No watermark yet: the first run starts at the earliest raw hour (5 hours ago), beyond
		// the 3-hour lookback, and never rolls up the open hour.
		rows, _ := e.admin.Query(e.ctx, `SELECT (date_trunc('hour', now(), 'UTC') - hour)::text || ':' || total || '/' || ok_count
			FROM result_rollups_hourly WHERE monitor_id = $1 ORDER BY hour`, mon)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"05:00:00:1/1", "02:00:00:2/1"}; !slices.Equal(got, want) {
			t.Fatalf("rollups (age:total/ok) = %v, want %v", got, want)
		}
		if count(`SELECT count(*) FROM rollup_state WHERE watermark = date_trunc('hour', now(), 'UTC')`) != 1 {
			t.Fatal("watermark is not the start of the current hour")
		}
	})

	t.Run("retention keeps and counts a partition with unrolled data, drops it once rolled up", func(t *testing.T) {
		mon := e.monitor(tenantA, "Old")
		withRollup, unrolled := "check_results_p202402", "check_results_p202403"
		for i, name := range []string{withRollup, unrolled} {
			lo := time.Date(2024, time.Month(2+i), 1, 0, 0, 0, 0, time.UTC)
			pgtest.MustExec(t, e.admin,
				fmt.Sprintf(`CREATE TABLE %s PARTITION OF check_results FOR VALUES FROM ('%s') TO ('%s')`, name, lo.Format(time.RFC3339), lo.AddDate(0, 1, 0).Format(time.RFC3339)),
				`ALTER TABLE `+name+` OWNER TO maiak_owner`,
				fmt.Sprintf(`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, latency_ms) VALUES ('%s', '%s', '%s', '%s', true, 10)`,
					tenantA, mon, homeLocation, lo.Add(3*time.Hour).Format(time.RFC3339)))
		}
		// The first month was rolled up (a backfill with the explicit range; the catch-up never
		// looks back more than 7 days); the second was not.
		worker := e.Conn(t, e.WorkerURL)
		if _, err := worker.Exec(e.ctx, `SELECT compute_rollups('2024-02-01T00:00:00Z', '2024-03-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		before := metric(t, reg, "maiak_jobs_partitions_kept_total", "rollups_incomplete")
		if got := h.RunTask(e.ctx, task("drop_expired_partitions")); got != "ok" {
			t.Fatal(got)
		}
		if count(`SELECT count(*) FROM pg_class WHERE relname = $1`, withRollup) != 0 {
			t.Fatal("rolled-up expired partition kept")
		}
		if count(`SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE c.relname = $1`, unrolled) != 1 {
			t.Fatal("partition with unrolled data was detached or dropped")
		}
		if got := metric(t, reg, "maiak_jobs_partitions_kept_total", "rollups_incomplete"); got != before+1 {
			t.Fatalf("kept metric = %v, want %v", got, before+1)
		}
		e.logMu.Lock()
		logs := e.logs.String()
		e.logMu.Unlock()
		if !strings.Contains(logs, `"msg":"retention kept an expired partition","partition":"`+unrolled+`","reason":"rollups_incomplete"`) {
			t.Fatalf("kept partition not logged:\n%s", logs)
		}

		// Once its hours are rolled up, the next run drops it and counts nothing more.
		if _, err := worker.Exec(e.ctx, `SELECT compute_rollups('2024-03-01T00:00:00Z', '2024-04-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		if got := h.RunTask(e.ctx, task("drop_expired_partitions")); got != "ok" {
			t.Fatal(got)
		}
		if count(`SELECT count(*) FROM pg_class WHERE relname = $1`, unrolled) != 0 {
			t.Fatal("partition kept after its rollups were completed")
		}
		if got := metric(t, reg, "maiak_jobs_partitions_kept_total", "rollups_incomplete"); got != before+1 {
			t.Fatalf("kept metric after the drop = %v, want %v", got, before+1)
		}
	})

	t.Run("drop_expired_partitions drops an old empty partition", func(t *testing.T) {
		old := "check_results_p202401"
		pgtest.MustExec(t, e.admin,
			`CREATE TABLE `+old+` PARTITION OF check_results FOR VALUES FROM ('2024-01-01T00:00:00Z') TO ('2024-02-01T00:00:00Z')`,
			`ALTER TABLE `+old+` OWNER TO maiak_owner`)
		if got := h.RunTask(e.ctx, task("drop_expired_partitions")); got != "ok" {
			t.Fatal(got)
		}
		if count(`SELECT count(*) FROM pg_class WHERE relname = $1`, old) != 0 {
			t.Fatal("expired partition kept")
		}
		if got := metric(t, reg, "maiak_jobs_housekeeping_last_success_timestamp_seconds", "drop_expired_partitions"); got < float64(time.Now().Add(-time.Minute).Unix()) {
			t.Fatalf("last success = %v", got)
		}
	})

	t.Run("Run executes every task at start and stops on cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(e.ctx)
		done := make(chan error, 1)
		go func() { done <- h.Run(ctx) }()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if metric(t, reg, "maiak_jobs_housekeeping_runs_total", "ensure_partitions", "ok") >= 2 &&
				metric(t, reg, "maiak_jobs_housekeeping_runs_total", "purge_expired_sessions", "ok") >= 2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"ensure_partitions", "compute_rollups", "drop_expired_partitions", "purge_expired_sessions"} {
			if metric(t, reg, "maiak_jobs_housekeeping_runs_total", name, "ok") < 2 {
				t.Errorf("%s did not run at start", name)
			}
		}
	})
}

type blockingNotifier struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingNotifier) Send(ctx context.Context, _, _ string) error {
	b.started <- struct{}{}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return &notify.SendError{Kind: notify.KindTimeout}
	}
}

// Shutdown stops claiming but lets a send in flight finish and be recorded; a send that outlives
// the drain timeout is abandoned without recording a failure (its lease expires).
func TestIntegrationGracefulShutdown(t *testing.T) {
	e := newEnv(t, "jobsshutdown")
	run := func(drain time.Duration, n *blockingNotifier) (uuid.UUID, func() error, context.CancelFunc) {
		id := e.deliver(tenantA, e.monitor(tenantA, "S"), notify.DeliveryResolved, map[string]any{"monitorName": "S", "durationSeconds": 1})
		d := jobs.NewDeliverer(jobs.DeliveryConfig{PublicURL: "https://maiak.example.com", DrainTimeout: drain, PollInterval: 20 * time.Millisecond}, jobs.DeliveryDeps{
			Pool: e.Pool(t, e.WorkerURL, 4), Channels: notify.NewService(e.Pool(t, e.WorkerURL, 2), e.keys, nil, ""),
			Notifier: n, Metrics: jobs.NewMetrics(prometheus.NewRegistry()), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		ctx, cancel := context.WithCancel(e.ctx)
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		return id, func() error { return <-done }, cancel
	}

	n := &blockingNotifier{started: make(chan struct{}, 1), release: make(chan struct{})}
	id, wait, cancel := run(5*time.Second, n)
	<-n.started
	cancel()
	time.Sleep(100 * time.Millisecond)
	close(n.release)
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if r := e.row(id); r.status != "sent" {
		t.Fatalf("in-flight send not recorded at shutdown: %+v", r)
	}

	n = &blockingNotifier{started: make(chan struct{}, 1), release: make(chan struct{})}
	id, wait, cancel = run(100*time.Millisecond, n)
	<-n.started
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if r := e.row(id); r.status != "in_progress" || r.attempts != 0 {
		t.Fatalf("abandoned send was recorded: %+v", r)
	}
}
