package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// The request logs of a login, a monitor create and a Slack PUT carry request_id, tenant_id,
// method, route template, status and duration, and none of the secrets those requests carry.
func TestIntegrationRequestLogsCarryNoSecrets(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apilogs"))
	ctx := context.Background()
	logs := &lockedBuffer{}
	log := logging.New(logs, slog.LevelDebug) // the production logger, at its most verbose

	pool := env.Pool(t, env.APIURL, 4)
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
	h, err := api.NewHandler(api.Config{Version: "test", Commit: "test", CookieSecure: true}, api.Deps{
		Auth: svc, Monitors: monitors.NewService(pool, sched, incidents.NewService(sched)), Queries: queries.New(pool, nil),
		Slack: notify.NewService(pool, keys, &fakeSlack{}, "https://maiak.example.com"), Logger: log,
		Metrics: metrics.NewHTTP(prometheus.NewRegistry()),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := &client{t: t, srv: srv}

	r := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"`+adminLogin+`","password":"`+adminPassword+`"}`, map[string]string{"X-Request-Id": "req-login-1"})
	if r.code != http.StatusOK || r.header.Get("X-Request-Id") != "req-login-1" {
		t.Fatalf("login: %d %s (request id %q)", r.code, r.raw, r.header.Get("X-Request-Id"))
	}
	token := sessionCookie(t, r).Value
	csrf, _ := r.body["csrf_token"].(string)
	unsafe := map[string]string{"Cookie": api.SessionCookie + "=" + token, "X-CSRF-Token": csrf}

	const monitorURL = "https://status.example.com/health?apikey=monitor-url-secret-123"
	m := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Logged","url":"`+monitorURL+`"}`, unsafe)
	if m.code != http.StatusCreated || m.header.Get("X-Request-Id") == "" {
		t.Fatalf("create: %d %s", m.code, m.raw)
	}
	if s := c.do(http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"`+slackURL+`"}`, unsafe); s.code != http.StatusOK {
		t.Fatalf("slack put: %d %s", s.code, s.raw)
	}
	// A rejected value must not be echoed into the logs either.
	if s := c.do(http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"https://evil.example.com/rejected-webhook-secret"}`, unsafe); s.code != http.StatusUnprocessableEntity {
		t.Fatalf("bad slack put: %d %s", s.code, s.raw)
	}
	if g := c.do(http.MethodGet, "/api/v1/monitors/"+m.body["id"].(string), "", unsafe); g.code != http.StatusOK {
		t.Fatalf("get: %d", g.code)
	}

	out := logs.String()
	for _, secret := range []string{adminPassword, token, csrf, monitorURL, "monitor-url-secret-123", "status.example.com",
		slackURL, "xoxSECRETTOKEN", "T0SECRET", "rejected-webhook-secret", "evil.example.com"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}

	var tenantID string
	if err := env.AdminConn(t).QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = 'default'`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"POST /api/v1/auth/login 200":          false,
		"POST /api/v1/monitors 201":            false,
		"PUT /api/v1/settings/slack 200":       false,
		"PUT /api/v1/settings/slack 422":       false,
		"GET /api/v1/monitors/{monitorId} 200": false,
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not a JSON log line: %s", line)
		}
		if e["msg"] != "http request" {
			continue
		}
		key := e["method"].(string) + " " + e["route"].(string) + " " + jsonInt(e["status"])
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected request log: %s", line)
			continue
		}
		want[key] = true
		if id, _ := e["request_id"].(string); id == "" || (key == "POST /api/v1/auth/login 200" && id != "req-login-1") {
			t.Errorf("request_id missing or wrong: %s", line)
		}
		if e["tenant_id"] != tenantID {
			t.Errorf("tenant_id missing: %s", line)
		}
		if _, ok := e["duration_ms"].(float64); !ok {
			t.Errorf("duration_ms missing: %s", line)
		}
		if _, ok := e["path"]; ok {
			t.Errorf("raw path logged: %s", line)
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("no request log for %s", k)
		}
	}
}

func jsonInt(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
