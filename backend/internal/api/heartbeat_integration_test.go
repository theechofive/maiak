package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const heartbeatPublicURL = "https://maiak.example.com"

// newHeartbeatServer is newServer with MAIAK_PUBLIC_URL, a fake clock for the ping limits and the
// production logger writing into logs.
func newHeartbeatServer(t *testing.T, env *pgtest.Env, logs *lockedBuffer, clk clock.Clock) *client {
	t.Helper()
	ctx := context.Background()
	pool := env.Pool(t, env.APIURL, 8)
	log := logging.New(logs, slog.LevelDebug)
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
	sched := scheduler.New()
	h, err := api.NewHandler(api.Config{Version: "test", Commit: "test", CookieSecure: true, TrustedProxyHops: 1, PublicURL: heartbeatPublicURL},
		api.Deps{Auth: svc, Monitors: monitors.NewService(pool, sched, incidents.NewService(sched)), Queries: queries.New(pool, nil),
			Plans: plans.NewService(pool), Logger: log, Clock: clk, Pings: api.NewPingMetrics(prometheus.NewRegistry())})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

// tokenOf returns the path and token of a heartbeat URL.
func tokenOf(t *testing.T, url any) (string, string) {
	t.Helper()
	s, _ := url.(string)
	if !strings.HasPrefix(s, heartbeatPublicURL+monitors.HeartbeatPath) {
		t.Fatalf("heartbeat URL %q", s)
	}
	path := strings.TrimPrefix(s, heartbeatPublicURL)
	return path, strings.TrimPrefix(path, monitors.HeartbeatPath)
}

func TestIntegrationHeartbeatAPI(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiheartbeat"))
	logs := &lockedBuffer{}
	clk := clock.NewFake(time.Now())
	c := newHeartbeatServer(t, env, logs, clk)
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	admin := env.AdminConn(t)
	ctx := context.Background()
	xff := func(ip string) map[string]string { return map[string]string{"X-Forwarded-For": ip} }

	// Create: defaults, one-time URL, no URL fields.
	r := c.do(http.MethodPost, "/api/v1/monitors", `{"type":"heartbeat","name":"Alertmanager"}`, unsafe)
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	id, _ := r.body["id"].(string)
	want := map[string]any{
		"type": "heartbeat", "name": "Alertmanager", "url": "", "status": "unknown", "paused": false,
		"expectedIntervalSeconds": 300.0, "intervalSeconds": 300.0, "graceSeconds": 60.0, "failureThreshold": 1.0,
		"lastPingAt": nil, "notifySlack": true, "ongoingIncident": nil,
	}
	for k, v := range want {
		if r.body[k] != v {
			t.Errorf("create %s = %v, want %v", k, r.body[k], v)
		}
	}
	pingPath, token := tokenOf(t, r.body["heartbeatUrl"])
	tokens := []string{token}
	sum := sha256.Sum256([]byte(token))
	if n := countRows(t, env, `SELECT count(*) FROM heartbeat_tokens WHERE token_hash = $1`, sum[:]); n != 1 {
		t.Fatalf("stored hashes = %d", n)
	}
	var next string
	if err := admin.QueryRow(ctx, `SELECT next_run_at::text FROM monitor_location_state WHERE monitor_id = $1`, id).Scan(&next); err != nil || next != "infinity" {
		t.Fatalf("schedule = %q %v", next, err)
	}
	// The URL is shown once: reads never carry it.
	for _, path := range []string{"/api/v1/monitors/" + id, "/api/v1/monitors"} {
		if g := c.do(http.MethodGet, path, "", cookie); g.code != http.StatusOK || strings.Contains(g.raw, token) || strings.Contains(g.raw, "heartbeatUrl") {
			t.Fatalf("GET %s: %d %s", path, g.code, g.raw)
		}
	}

	t.Run("validation", func(t *testing.T) {
		for body, field := range map[string]string{
			`{"type":"heartbeat","name":"x","url":"https://example.com"}`: "url",
			`{"type":"heartbeat","name":"x","expectedStatus":"2xx"}`:      "expectedStatus",
			`{"name":"x","url":"https://example.com","graceSeconds":10}`:  "graceSeconds",
			`{"name":"x"}`: "url",
		} {
			if v := c.do(http.MethodPost, "/api/v1/monitors", body, unsafe); v.code != http.StatusUnprocessableEntity || !strings.Contains(v.raw, `"field":"`+field+`"`) {
				t.Errorf("%s: %d %s", body, v.code, v.raw)
			}
		}
		for _, body := range []string{
			`{"type":"heartbeat","name":"x","expectedIntervalSeconds":59}`,
			`{"type":"heartbeat","name":"x","expectedIntervalSeconds":86401}`,
			`{"type":"heartbeat","name":"x","graceSeconds":-1}`,
			`{"type":"heartbeat","name":"x","graceSeconds":3601}`,
			`{"type":"tcp","name":"x"}`,
		} {
			if v := c.do(http.MethodPost, "/api/v1/monitors", body, unsafe); v.code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", body, v.code, v.raw)
			}
		}
		// PATCH: heartbeat fields change, failureThreshold is ignored, http fields are refused.
		p := c.do(http.MethodPatch, "/api/v1/monitors/"+id, `{"expectedIntervalSeconds":60,"graceSeconds":0,"failureThreshold":3}`, unsafe)
		if p.code != http.StatusOK || p.body["expectedIntervalSeconds"] != 60.0 || p.body["graceSeconds"] != 0.0 || p.body["failureThreshold"] != 1.0 {
			t.Fatalf("patch: %d %s", p.code, p.raw)
		}
		if p := c.do(http.MethodPatch, "/api/v1/monitors/"+id, `{"url":"https://example.com"}`, unsafe); p.code != http.StatusUnprocessableEntity {
			t.Fatalf("patch url: %d %s", p.code, p.raw)
		}
		c.do(http.MethodPatch, "/api/v1/monitors/"+id, `{"expectedIntervalSeconds":300,"graceSeconds":60}`, unsafe)
	})

	t.Run("pings", func(t *testing.T) {
		unknownBodies := map[string]bool{}
		for i, p := range []string{monitors.HeartbeatPath + "x", monitors.HeartbeatPath + strings.Repeat("A", 43), monitors.HeartbeatPath + token + "x",
			monitors.HeartbeatPath + strings.ToUpper(token), monitors.HeartbeatPath + "%2e%2e"} {
			u := c.do(http.MethodGet, p, "", xff(fmt.Sprintf("198.51.100.%d", i)))
			if u.code != http.StatusNotFound {
				t.Fatalf("unknown %s: %d %s", p, u.code, u.raw)
			}
			delete(u.body, "request_id")
			unknownBodies[fmt.Sprint(u.body)] = true
		}
		if len(unknownBodies) != 1 {
			t.Fatalf("unknown tokens answered differently: %v", unknownBodies)
		}

		g := c.do(http.MethodGet, pingPath, "", xff("203.0.113.9"))
		if g.code != http.StatusNoContent || g.raw != "" {
			t.Fatalf("ping: %d %q", g.code, g.raw)
		}
		// A replay within the same second is refused; the next second it is accepted, by POST
		// with an Alertmanager body that is ignored.
		if rr := c.do(http.MethodGet, pingPath, "", xff("203.0.113.9")); rr.code != http.StatusTooManyRequests || rr.header.Get("Retry-After") != "1" {
			t.Fatalf("replay: %d %s", rr.code, rr.raw)
		}
		clk.Advance(time.Second)
		body := `{"version":"4","status":"firing","alerts":[{"labels":{"alertname":"Watchdog"}}]}`
		if p := c.do(http.MethodPost, pingPath, body, xff("203.0.113.9")); p.code != http.StatusNoContent {
			t.Fatalf("POST ping: %d %s", p.code, p.raw)
		}
		// POST with no CSRF token and no session is fine; with a session it is just as fine.
		clk.Advance(time.Second)
		if p := c.do(http.MethodPost, pingPath, "", cookie); p.code != http.StatusNoContent {
			t.Fatalf("POST ping with a session: %d %s", p.code, p.raw)
		}
		d := c.do(http.MethodGet, "/api/v1/monitors/"+id, "", cookie)
		if d.body["lastPingAt"] == nil {
			t.Fatalf("lastPingAt not set: %s", d.raw)
		}
	})

	t.Run("unknown tokens are limited per client IP", func(t *testing.T) {
		for i := range 30 {
			if u := c.do(http.MethodGet, monitors.HeartbeatPath+fmt.Sprintf("guess-%d", i), "", xff("192.0.2.50")); u.code != http.StatusNotFound {
				t.Fatalf("guess %d: %d", i, u.code)
			}
		}
		if u := c.do(http.MethodGet, monitors.HeartbeatPath+"guess-x", "", xff("192.0.2.50")); u.code != http.StatusTooManyRequests {
			t.Fatalf("over the limit: %d %s", u.code, u.raw)
		}
		clk.Advance(time.Second)
		if g := c.do(http.MethodGet, pingPath, "", xff("192.0.2.51")); g.code != http.StatusNoContent {
			t.Fatalf("valid ping from another IP: %d", g.code)
		}
	})

	t.Run("regenerate", func(t *testing.T) {
		if rr := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/heartbeat-token", "", cookie); rr.code != http.StatusForbidden {
			t.Fatalf("without CSRF: %d", rr.code)
		}
		rr := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/heartbeat-token", "", unsafe)
		if rr.code != http.StatusOK {
			t.Fatalf("regenerate: %d %s", rr.code, rr.raw)
		}
		newPath, newToken := tokenOf(t, rr.body["heartbeatUrl"])
		if newToken == token {
			t.Fatal("same token")
		}
		clk.Advance(time.Second)
		if o := c.do(http.MethodGet, pingPath, "", xff("203.0.113.10")); o.code != http.StatusNotFound {
			t.Fatalf("old URL: %d", o.code)
		}
		if n := c.do(http.MethodGet, newPath, "", xff("203.0.113.10")); n.code != http.StatusNoContent {
			t.Fatalf("new URL: %d", n.code)
		}
		h := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Site","url":"https://example.com"}`, unsafe)
		hid, _ := h.body["id"].(string)
		if x := c.do(http.MethodPost, "/api/v1/monitors/"+hid+"/heartbeat-token", "", unsafe); x.code != http.StatusConflict || x.body["type"] != "urn:maiak:problem:not-a-heartbeat" {
			t.Fatalf("regenerate an http monitor: %d %s", x.code, x.raw)
		}
		if h.body["type"] != "http" || h.body["expectedIntervalSeconds"] != nil || h.body["graceSeconds"] != nil || h.body["lastPingAt"] != nil {
			t.Fatalf("http monitor heartbeat fields: %s", h.raw)
		}
		if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE action = 'monitor.heartbeat_token_regenerated' AND entity_id = $1`, id); n != 1 {
			t.Fatalf("audit rows = %d", n)
		}
		tokens = append(tokens, newToken)
		pingPath = newPath
	})

	t.Run("paused and deleted heartbeats answer 204 but record nothing", func(t *testing.T) {
		var before *time.Time
		if err := admin.QueryRow(ctx, `SELECT last_ping_at FROM heartbeat_tokens WHERE monitor_id = $1`, id).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if p := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/pause", "", unsafe); p.code != http.StatusOK {
			t.Fatalf("pause: %d", p.code)
		}
		clk.Advance(time.Second)
		if g := c.do(http.MethodGet, pingPath, "", xff("203.0.113.11")); g.code != http.StatusNoContent {
			t.Fatalf("paused ping: %d", g.code)
		}
		var after *time.Time
		if err := admin.QueryRow(ctx, `SELECT last_ping_at FROM heartbeat_tokens WHERE monitor_id = $1`, id).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if before == nil || after == nil || !after.Equal(*before) {
			t.Fatalf("paused ping recorded: %v -> %v", before, after)
		}
		// Resume restarts the wait.
		pgtest.MustExec(t, admin, `UPDATE heartbeat_tokens SET waiting_since = now() - interval '1 day' WHERE monitor_id = '`+id+`'`)
		if p := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/resume", "", unsafe); p.code != http.StatusOK || p.body["status"] != "unknown" {
			t.Fatalf("resume: %d %s", p.code, p.raw)
		}
		var wait time.Duration
		var applied bool
		if err := admin.QueryRow(ctx, `SELECT now() - waiting_since, applied_ping_at IS NOT DISTINCT FROM last_ping_at FROM heartbeat_tokens WHERE monitor_id = $1`, id).Scan(&wait, &applied); err != nil || wait > time.Minute || !applied {
			t.Fatalf("after resume: waited %v, old ping applied %t (%v)", wait, applied, err)
		}
	})

	t.Run("quota: heartbeats and http monitors are counted apart", func(t *testing.T) {
		pgtest.MustExec(t, admin, `UPDATE tenants SET limits = '{"max_heartbeats": 2}' WHERE slug = 'default'`)
		u := c.do(http.MethodGet, "/api/v1/usage", "", cookie)
		if usage, _ := u.body["usage"].(map[string]any); usage["heartbeats"] != 1.0 || usage["monitors"] != 1.0 {
			t.Fatalf("usage: %s", u.raw)
		}
		// Concurrent creates at the last free slot: exactly one wins.
		var wg sync.WaitGroup
		codes := make([]int, 4)
		bodies := make([]string, 4)
		for i := range codes {
			wg.Go(func() {
				res := c.do(http.MethodPost, "/api/v1/monitors", fmt.Sprintf(`{"type":"heartbeat","name":"hb %d"}`, i), unsafe)
				codes[i], bodies[i] = res.code, res.raw
			})
		}
		wg.Wait()
		created := 0
		for i, code := range codes {
			switch {
			case code == http.StatusCreated:
				created++
			case code == http.StatusConflict && strings.Contains(bodies[i], `"limit":"heartbeats"`) && strings.Contains(bodies[i], `"max":2`):
			default:
				t.Errorf("create %d: %d %s", i, code, bodies[i])
			}
		}
		if created != 1 {
			t.Fatalf("created %d heartbeats at the limit, want 1", created)
		}
		// The http quota is separate.
		if h := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Second","url":"https://example.org"}`, unsafe); h.code != http.StatusCreated {
			t.Fatalf("http create next to full heartbeats: %d %s", h.code, h.raw)
		}
		// Deleting frees the slot.
		if d := c.do(http.MethodDelete, "/api/v1/monitors/"+id, "", unsafe); d.code != http.StatusNoContent {
			t.Fatalf("delete: %d", d.code)
		}
		clk.Advance(time.Second)
		if g := c.do(http.MethodGet, pingPath, "", xff("203.0.113.12")); g.code != http.StatusNoContent {
			t.Fatalf("deleted heartbeat ping: %d", g.code)
		}
		if h := c.do(http.MethodPost, "/api/v1/monitors", `{"type":"heartbeat","name":"again"}`, unsafe); h.code != http.StatusCreated {
			t.Fatalf("create after delete: %d %s", h.code, h.raw)
		}
	})

	// Neither the token nor its hash reaches the logs or an error response.
	out := logs.String()
	for _, tok := range tokens {
		sum := sha256.Sum256([]byte(tok))
		for _, secret := range []string{tok, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:]), base64.RawURLEncoding.EncodeToString(sum[:])} {
			if strings.Contains(out, secret) {
				t.Errorf("logs contain %q", secret)
			}
		}
	}
	if !strings.Contains(out, `"route":"/api/v1/heartbeat/{token}"`) {
		t.Errorf("access log without the ping route template")
	}
}
