package api_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const slackURL = "https://hooks.slack.com/services/T0SECRET/B0SECRET/xoxSECRETTOKEN"

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type fakeSlack struct {
	mu   sync.Mutex
	err  error
	urls []string
}

func (f *fakeSlack) Send(_ context.Context, u, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, u)
	return f.err
}

// newSlackServer is newServer with the Slack settings wired and the log captured.
func newSlackServer(t *testing.T, env *pgtest.Env, sender *fakeSlack, logs *lockedBuffer, clk clock.Clock) *client {
	t.Helper()
	ctx := context.Background()
	pool := env.Pool(t, env.APIURL, 4)
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, err := auth.NewService(pool, auth.Options{SessionTTL: time.Hour, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := crypto.HashPassword(adminPassword, crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BootstrapAdmin(ctx, adminLogin, hash); err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New()
	h, err := api.NewHandler(api.Config{Version: "test", Commit: "test", CookieSecure: true, TrustedProxyHops: 1}, api.Deps{
		Auth: svc, Monitors: monitors.NewService(pool, sched, incidents.NewService(sched)),
		Slack: notify.NewService(pool, keys, sender, "https://maiak.example.com"), Logger: log, Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

func TestIntegrationSlackSettingsAPI(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apislack"))
	sender := &fakeSlack{}
	logs := &lockedBuffer{}
	clk := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	c := newSlackServer(t, env, sender, logs, clk)
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	var bodies []string
	// Every call of the test endpoint first waits out the per-tenant limit of one per 5 s.
	record := func(r response) response { bodies = append(bodies, r.raw); clk.Advance(5 * time.Second); return r }

	if r := record(c.do(http.MethodGet, "/api/v1/settings/slack", "", nil)); r.code != http.StatusUnauthorized {
		t.Fatalf("get without session: %d", r.code)
	}
	if r := record(c.do(http.MethodGet, "/api/v1/settings/slack", "", cookie)); r.code != 200 || r.raw != `{"configured":false}`+"\n" {
		t.Fatalf("get empty: %d %q", r.code, r.raw)
	}
	if r := record(c.do(http.MethodPost, "/api/v1/settings/slack/test", "", unsafe)); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:slack-not-configured" {
		t.Fatalf("test unconfigured: %d %s", r.code, r.raw)
	}

	// CSRF is required for both unsafe operations.
	for _, path := range []string{"/api/v1/settings/slack", "/api/v1/settings/slack/test"} {
		method := http.MethodPut
		if strings.HasSuffix(path, "/test") {
			method = http.MethodPost
		}
		r := record(c.do(method, path, `{"webhookUrl":"`+slackURL+`"}`, cookie))
		if r.code != http.StatusForbidden {
			t.Fatalf("%s %s without CSRF: %d", method, path, r.code)
		}
	}

	// Domain validation: 422 with fieldErrors, value not echoed.
	for _, bad := range []string{"https://evil.example/services/T0SECRET/x", "http://hooks.slack.com/services/T0SECRET/x",
		"https://hooks.slack.com/services/T0SECRET/" + strings.Repeat("a", 300)} {
		r := record(c.do(http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"`+bad+`"}`, unsafe))
		fe, _ := r.body["fieldErrors"].([]any)
		if r.code != http.StatusUnprocessableEntity || len(fe) != 1 || fe[0].(map[string]any)["field"] != "webhookUrl" {
			t.Fatalf("bad url %q: %d %s", bad, r.code, r.raw)
		}
	}
	if r := record(c.do(http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"`+slackURL+`","extra":1}`, unsafe)); r.code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", r.code)
	}

	r := record(c.do(http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"`+slackURL+`"}`, unsafe))
	if r.code != 200 || r.body["configured"] != true || r.body["status"] != "active" || len(r.body) != 2 {
		t.Fatalf("put: %d %s", r.code, r.raw)
	}
	if r := record(c.do(http.MethodGet, "/api/v1/settings/slack", "", cookie)); r.code != 200 || r.body["status"] != "active" {
		t.Fatalf("get: %d %s", r.code, r.raw)
	}
	if r := record(c.do(http.MethodPost, "/api/v1/settings/slack/test", "", unsafe)); r.code != http.StatusNoContent {
		t.Fatalf("test: %d %s", r.code, r.raw)
	}
	if len(sender.urls) != 1 || sender.urls[0] != slackURL {
		t.Fatalf("sent to %v", sender.urls)
	}

	sender.err = &notify.SendError{Kind: notify.KindHTTPStatus, Status: 500}
	if r := record(c.do(http.MethodPost, "/api/v1/settings/slack/test", "", unsafe)); r.code != http.StatusBadGateway || r.body["type"] != "urn:maiak:problem:slack-delivery-failed" {
		t.Fatalf("test 500: %d %s", r.code, r.raw)
	}
	sender.err = &notify.SendError{Kind: notify.KindRateLimited, Status: 429, RetryAfter: 30 * time.Second}
	if r := record(c.do(http.MethodPost, "/api/v1/settings/slack/test", "", unsafe)); r.code != http.StatusTooManyRequests || r.header.Get("Retry-After") != "30" {
		t.Fatalf("test 429: %d %s", r.code, r.raw)
	}
	sender.err = &notify.SendError{Kind: notify.KindNotFound, Status: 404, Permanent: true}
	if r := record(c.do(http.MethodPost, "/api/v1/settings/slack/test", "", unsafe)); r.code != http.StatusBadGateway {
		t.Fatalf("test 404: %d %s", r.code, r.raw)
	}
	if r := record(c.do(http.MethodGet, "/api/v1/settings/slack", "", cookie)); r.body["status"] != "failed" {
		t.Fatalf("status after 404: %s", r.raw)
	}

	// The URL never appears in a response or a log line.
	all := strings.Join(bodies, "\n") + logs.String()
	for _, secret := range []string{"T0SECRET", "xoxSECRETTOKEN"} {
		if strings.Contains(all, secret) {
			t.Fatalf("%s leaked in responses or logs:\n%s", secret, all)
		}
	}
	if logs.String() == "" {
		t.Fatal("no log output captured")
	}
}

// POST /settings/slack/test is limited to one call per 5 s per tenant, whatever the outcome.
func TestIntegrationSlackTestRateLimit(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apislacklimit"))
	sender := &fakeSlack{}
	clk := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	c := newSlackServer(t, env, sender, &lockedBuffer{}, clk)
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	if r := c.do(http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"`+slackURL+`"}`, unsafe); r.code != 200 {
		t.Fatalf("put: %d %s", r.code, r.raw)
	}
	test := func(hdr map[string]string) response {
		return c.do(http.MethodPost, "/api/v1/settings/slack/test", "", hdr)
	}
	if r := test(unsafe); r.code != http.StatusNoContent {
		t.Fatalf("first test: %d %s", r.code, r.raw)
	}
	clk.Advance(2 * time.Second)
	r := test(unsafe)
	if r.code != http.StatusTooManyRequests || r.body["type"] != "urn:maiak:problem:rate-limited" || r.header.Get("Retry-After") != "3" ||
		!strings.HasPrefix(r.header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("second test within 5 s: %d %v %s", r.code, r.header, r.raw)
	}
	if len(sender.urls) != 1 {
		t.Fatalf("limited call reached Slack: %d sends", len(sender.urls))
	}
	// Another tenant has its own budget (its call reaches the handler: 409, not configured).
	other, otherCSRF := otherTenantSession(t, env)
	if r := test(withCSRF(other, otherCSRF)); r.code != http.StatusConflict {
		t.Fatalf("other tenant: %d %s", r.code, r.raw)
	}
	clk.Advance(3 * time.Second)
	if r := test(unsafe); r.code != http.StatusNoContent || len(sender.urls) != 2 {
		t.Fatalf("after 5 s: %d %s, %d sends", r.code, r.raw, len(sender.urls))
	}
}
