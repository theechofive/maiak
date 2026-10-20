package api_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// withCSRF returns the session headers plus the CSRF token for unsafe requests.
func withCSRF(cookie map[string]string, csrf string) map[string]string {
	return map[string]string{"Cookie": cookie["Cookie"], "X-CSRF-Token": csrf}
}

func TestIntegrationMonitorLifecycle(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apimonitors"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	admin := env.AdminConn(t)
	ctx := context.Background()

	r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":" Website ","url":"HTTPS://Example.com/health?token=s3cr3t"}`, unsafe)
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	id, _ := r.body["id"].(string)
	want := map[string]any{
		"name": "Website", "url": "https://example.com/health?token=s3cr3t", "status": "unknown", "paused": false,
		"intervalSeconds": 60.0, "timeoutSeconds": 10.0, "failureThreshold": 2.0, "expectedStatus": "2xx",
		"notifySlack": true, "lastErrorKind": nil, "lastCheckedAt": nil, "tlsExpiresAt": nil, "ongoingIncident": nil,
	}
	for k, v := range want {
		if r.body[k] != v {
			t.Errorf("create %s = %v, want %v", k, r.body[k], v)
		}
	}
	if up, _ := r.body["uptime"].(map[string]any); up == nil || up["h24"] != nil {
		t.Errorf("uptime = %v", r.body["uptime"])
	}

	// Initial rows: home location, schedule within the jitter window, state unknown.
	var code, lsStatus, msStatus string
	var delay time.Duration
	var failures int
	if err := admin.QueryRow(ctx, `
		SELECT l.code, s.status, s.consecutive_failures, s.next_run_at - now(), ms.status
		FROM monitor_location_state s
		JOIN probe_locations l ON l.id = s.location_id
		JOIN monitor_state ms ON ms.monitor_id = s.monitor_id
		WHERE s.monitor_id = $1`, id).Scan(&code, &lsStatus, &failures, &delay, &msStatus); err != nil {
		t.Fatal(err)
	}
	if code != "home-eu-central-1" || lsStatus != "unknown" || failures != 0 || msStatus != "unknown" || delay < -time.Second || delay > 30*time.Second {
		t.Fatalf("initial state: %s %s %d %v %s", code, lsStatus, failures, delay, msStatus)
	}

	list := c.do(http.MethodGet, "/api/v1/monitors", "", cookie)
	items, _ := list.body["items"].([]any)
	if list.code != http.StatusOK || len(items) != 1 || fmt.Sprint(list.body["summary"]) != "map[down:0 paused:0 unknown:1 up:0]" {
		t.Fatalf("list: %d %s", list.code, list.raw)
	}
	if item := items[0].(map[string]any); item["uptime24h"] != nil || item["lastResponseMs"] != nil || item["id"] != id {
		t.Fatalf("list item: %v", item)
	}
	if g := c.do(http.MethodGet, "/api/v1/monitors/"+id, "", cookie); g.code != http.StatusOK || g.body["name"] != "Website" {
		t.Fatalf("get: %d %s", g.code, g.raw)
	}

	// Patch.
	p := c.do(http.MethodPatch, "/api/v1/monitors/"+id, `{"name":"Site","expectedStatus":"204","failureThreshold":3,"notifySlack":false,"intervalSeconds":60}`, unsafe)
	if p.code != http.StatusOK || p.body["name"] != "Site" || p.body["expectedStatus"] != "204" || p.body["failureThreshold"] != 3.0 || p.body["notifySlack"] != false {
		t.Fatalf("patch: %d %s", p.code, p.raw)
	}
	for body, field := range map[string]string{
		`{"intervalSeconds":30}`:       "intervalSeconds",
		`{"timeoutSeconds":5}`:         "timeoutSeconds",
		`{"url":"https://10.0.0.1/"}`:  "url",
		`{"url":"http://example.com"}`: "url",
		`{"expectedStatus":"3xx"}`:     "",
		`{"name":"   "}`:               "name",
	} {
		r := c.do(http.MethodPatch, "/api/v1/monitors/"+id, body, unsafe)
		if field == "" { // rejected by the contract (pattern) before the domain
			if r.code != http.StatusBadRequest {
				t.Errorf("patch %s: %d %s", body, r.code, r.raw)
			}
			continue
		}
		fe, _ := r.body["fieldErrors"].([]any)
		if r.code != http.StatusUnprocessableEntity || r.body["type"] != "urn:maiak:problem:validation" || len(fe) != 1 || fe[0].(map[string]any)["field"] != field {
			t.Errorf("patch %s: %d %s", body, r.code, r.raw)
		}
	}
	if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"x","url":"https://localhost/"}`, unsafe); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("create with internal host: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"`+strings.Repeat("a", 81)+`","url":"https://example.com"}`, unsafe); r.code != http.StatusBadRequest || r.body["fieldErrors"] == nil {
		t.Fatalf("create with long name: %d %s", r.code, r.raw)
	}

	// Pause, then simulate a failing location and an in-flight lease, then resume.
	pause := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/pause", "", unsafe)
	if pause.code != http.StatusOK || pause.body["paused"] != true {
		t.Fatalf("pause: %d %s", pause.code, pause.raw)
	}
	if again := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/pause", "", unsafe); again.code != http.StatusOK {
		t.Fatalf("pause again: %d", again.code)
	}
	if l := c.do(http.MethodGet, "/api/v1/monitors", "", cookie); fmt.Sprint(l.body["summary"]) != "map[down:0 paused:1 unknown:0 up:0]" {
		t.Fatalf("summary while paused: %s", l.raw)
	}
	pgtest.MustExec(t, admin,
		`UPDATE monitor_location_state SET status = 'down', consecutive_failures = 3, lease_token = gen_random_uuid(),
			lease_until = now() + interval '30 seconds', next_run_at = now() + interval '1 hour' WHERE monitor_id = '`+id+`'`,
		`UPDATE monitor_state SET status = 'down', last_error_kind = 'timeout', status_changed_at = now() - interval '1 day' WHERE monitor_id = '`+id+`'`)
	res := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/resume", "", unsafe)
	if res.code != http.StatusOK || res.body["paused"] != false || res.body["status"] != "unknown" || res.body["lastErrorKind"] != nil {
		t.Fatalf("resume: %d %s", res.code, res.raw)
	}
	var leased bool
	if err := admin.QueryRow(ctx, `SELECT status, consecutive_failures, next_run_at - now(), lease_token IS NOT NULL
		FROM monitor_location_state WHERE monitor_id = $1`, id).Scan(&lsStatus, &failures, &delay, &leased); err != nil {
		t.Fatal(err)
	}
	if lsStatus != "unknown" || failures != 0 || leased || delay < -time.Second || delay > 30*time.Second {
		t.Fatalf("after resume: %s %d %v leased=%v", lsStatus, failures, delay, leased)
	}
	if since, _ := time.Parse(time.RFC3339Nano, res.body["statusSince"].(string)); time.Since(since) > time.Minute {
		t.Fatalf("statusSince not reset: %v", res.body["statusSince"])
	}

	// Delete.
	if d := c.do(http.MethodDelete, "/api/v1/monitors/"+id, "", unsafe); d.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", d.code, d.raw)
	}
	for _, rq := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/monitors/" + id},
		{http.MethodDelete, "/api/v1/monitors/" + id},
		{http.MethodPost, "/api/v1/monitors/" + id + "/resume"},
	} {
		if r := c.do(rq.method, rq.path, "", unsafe); r.code != http.StatusNotFound || r.body["type"] != "urn:maiak:problem:not-found" {
			t.Errorf("%s %s after delete: %d %s", rq.method, rq.path, r.code, r.raw)
		}
	}
	if l := c.do(http.MethodGet, "/api/v1/monitors", "", cookie); l.raw != "" && !strings.Contains(l.raw, `"items":[]`) {
		t.Fatalf("list after delete: %s", l.raw)
	}
	var deleted bool
	if err := admin.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM monitors WHERE id = $1`, id).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("soft delete: %v %v", deleted, err)
	}

	// Audit trail: one row per effective change, no URL anywhere.
	rows, err := admin.Query(ctx, `SELECT action, metadata::text FROM audit_log WHERE entity_type = 'monitor' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, meta string
		if err := rows.Scan(&action, &meta); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(meta, "s3cr3t") || strings.Contains(meta, "example.com") {
			t.Errorf("audit metadata leaks the URL: %s", meta)
		}
		got = append(got, action)
	}
	wantAudit := "monitor.created,monitor.updated,monitor.paused,monitor.resumed,monitor.deleted"
	if strings.Join(got, ",") != wantAudit {
		t.Fatalf("audit = %s\nwant    %s", strings.Join(got, ","), wantAudit)
	}
}

func TestIntegrationMonitorLimitUnderConcurrency(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apimonlimit"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()
	pgtest.MustExec(t, env.AdminConn(t), `UPDATE tenants SET limits = '{"max_monitors": 5}' WHERE slug = 'default'`)

	const n = 20
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"name":"m%d","url":"https://example.com/%d"}`, i, i)
			req, _ := http.NewRequest(http.MethodPost, c.srv.URL+"/api/v1/monitors", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range withCSRF(cookie, csrf) {
				req.Header.Set(k, v)
			}
			resp, err := c.srv.Client().Do(req)
			if err != nil {
				codes <- -1
				return
			}
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for code := range codes {
		count[code]++
	}
	if count[http.StatusCreated] != 5 || count[http.StatusConflict] != n-5 {
		t.Fatalf("status codes = %v, want 5 created and %d conflicts", count, n-5)
	}
	var live int
	if err := env.AdminConn(t).QueryRow(context.Background(), `SELECT count(*) FROM monitors WHERE deleted_at IS NULL`).Scan(&live); err != nil || live != 5 {
		t.Fatalf("live monitors = %d (%v)", live, err)
	}

	// The limit problem says why; a deleted monitor frees its slot.
	r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"x","url":"https://example.com"}`, withCSRF(cookie, csrf))
	if r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:limit-reached" {
		t.Fatalf("over limit: %d %s", r.code, r.raw)
	}
	l := c.do(http.MethodGet, "/api/v1/monitors", "", cookie)
	first := l.body["items"].([]any)[0].(map[string]any)["id"].(string)
	if d := c.do(http.MethodDelete, "/api/v1/monitors/"+first, "", withCSRF(cookie, csrf)); d.code != http.StatusNoContent {
		t.Fatalf("delete: %d", d.code)
	}
	if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"x","url":"https://example.com"}`, withCSRF(cookie, csrf)); r.code != http.StatusCreated {
		t.Fatalf("create after delete: %d %s", r.code, r.raw)
	}
}

// otherTenantSession creates a second tenant with its own owner and session directly in the
// database (login always binds to the default tenant) and returns the session headers.
func otherTenantSession(t *testing.T, env *pgtest.Env) (map[string]string, string) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	const csrf = "other-tenant-csrf"
	_, err := env.AdminConn(t).Exec(context.Background(), `
		WITH t AS (INSERT INTO tenants (id, slug, name) VALUES ('01920000-0000-7000-8000-0000000000b0', 'other', 'Other') RETURNING id),
		     u AS (INSERT INTO users (id, email) VALUES ('01920000-0000-7000-8000-0000000000b1', 'other@example.com') RETURNING id),
		     m AS (INSERT INTO memberships (tenant_id, user_id, role) SELECT t.id, u.id, 'owner' FROM t, u RETURNING tenant_id, user_id)
		INSERT INTO sessions (id, user_id, tenant_id, token_hash, csrf_token, expires_at)
		SELECT '01920000-0000-7000-8000-0000000000b2', user_id, tenant_id, $1, $2, now() + interval '1 hour' FROM m`, sum[:], csrf)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": api.SessionCookie + "=" + token}, csrf
}

func TestIntegrationMonitorTenantIsolation(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apimoniso"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()
	r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Mine","url":"https://example.com"}`, withCSRF(cookie, csrf))
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	id := r.body["id"].(string)

	other, otherCSRF := otherTenantSession(t, env)
	if me := c.do(http.MethodGet, "/api/v1/auth/me", "", other); me.code != http.StatusOK {
		t.Fatalf("other tenant session: %d %s", me.code, me.raw)
	}
	if l := c.do(http.MethodGet, "/api/v1/monitors", "", other); l.code != http.StatusOK || !strings.Contains(l.raw, `"items":[]`) ||
		fmt.Sprint(l.body["summary"]) != "map[down:0 paused:0 unknown:0 up:0]" {
		t.Fatalf("other tenant list: %d %s", l.code, l.raw)
	}
	unsafe := withCSRF(other, otherCSRF)
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/monitors/" + id, ""},
		{http.MethodPatch, "/api/v1/monitors/" + id, `{"name":"Hijacked"}`},
		{http.MethodPost, "/api/v1/monitors/" + id + "/pause", ""},
		{http.MethodPost, "/api/v1/monitors/" + id + "/resume", ""},
		{http.MethodDelete, "/api/v1/monitors/" + id, ""},
	} {
		if r := c.do(rq.method, rq.path, rq.body, unsafe); r.code != http.StatusNotFound {
			t.Errorf("other tenant %s %s: %d %s", rq.method, rq.path, r.code, r.raw)
		}
	}
	g := c.do(http.MethodGet, "/api/v1/monitors/"+id, "", cookie)
	if g.code != http.StatusOK || g.body["name"] != "Mine" || g.body["paused"] != false {
		t.Fatalf("owner's monitor changed: %d %s", g.code, g.raw)
	}
}

// A monitorId that is not a UUID is a contract violation (400) on every {monitorId} operation.
func TestIntegrationMonitorIDMustBeUUID(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apimonid"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/monitors/nope", ""},
		{http.MethodPatch, "/api/v1/monitors/nope", "{}"},
		{http.MethodDelete, "/api/v1/monitors/nope", ""},
		{http.MethodPost, "/api/v1/monitors/nope/pause", ""},
		{http.MethodPost, "/api/v1/monitors/nope/resume", ""},
		{http.MethodGet, "/api/v1/monitors/nope/stats", ""},
		{http.MethodGet, "/api/v1/monitors/nope/checks", ""},
		{http.MethodGet, "/api/v1/monitors/nope/incidents", ""},
	} {
		if r := c.do(rq.method, rq.path, rq.body, withCSRF(cookie, csrf)); r.code != http.StatusBadRequest || r.body["type"] != "urn:maiak:problem:bad-request" {
			t.Errorf("%s %s: %d %s", rq.method, rq.path, r.code, r.raw)
		}
	}
}

// Without an active home location a create answers 503 problem+json (ADR-0018).
func TestIntegrationCreateWithoutLocationIs503(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apimonnoloc"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()
	pgtest.MustExec(t, env.AdminConn(t), `UPDATE probe_locations SET status = 'disabled'`)
	r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"x","url":"https://example.com"}`, withCSRF(cookie, csrf))
	if r.code != http.StatusServiceUnavailable || r.body["type"] != "urn:maiak:problem:unavailable" {
		t.Fatalf("create without location: %d %s", r.code, r.raw)
	}
}

// Pause drops the lease (fencing a check in flight) and a URL change restarts the monitor like
// resume, clears the TLS fields and resolves an open incident as url_changed without a
// notification (ADR-0018).
func TestIntegrationPauseFencesAndURLChangeRestarts(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apimonrestart"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	admin := env.AdminConn(t)
	ctx := context.Background()

	r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Site","url":"https://example.com/a"}`, unsafe)
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	id := r.body["id"].(string)
	const incident = "01920000-0000-7000-8000-0000000000c1"
	down := func() {
		pgtest.MustExec(t, admin,
			`UPDATE monitor_location_state SET status = 'down', consecutive_failures = 3, lease_token = gen_random_uuid(),
				lease_until = now() + interval '30 seconds', next_run_at = now() + interval '1 hour' WHERE monitor_id = '`+id+`'`,
			`UPDATE monitor_state SET status = 'down', last_error_kind = 'timeout', status_changed_at = now() - interval '1 day',
				tls_expires_at = now() + interval '10 days', tls_warned_not_after = now() + interval '10 days' WHERE monitor_id = '`+id+`'`)
	}
	type row struct {
		lsStatus, msStatus string
		failures           int
		leased, tls        bool
		delay              time.Duration
	}
	read := func() row {
		var x row
		if err := admin.QueryRow(ctx, `
			SELECT s.status, ms.status, s.consecutive_failures, s.lease_token IS NOT NULL,
			       ms.tls_expires_at IS NOT NULL OR ms.tls_warned_not_after IS NOT NULL, s.next_run_at - now()
			FROM monitor_location_state s JOIN monitor_state ms ON ms.monitor_id = s.monitor_id
			WHERE s.monitor_id = $1`, id).Scan(&x.lsStatus, &x.msStatus, &x.failures, &x.leased, &x.tls, &x.delay); err != nil {
			t.Fatal(err)
		}
		return x
	}

	// Pause with a check in flight: the lease goes, the status reads unknown, TLS stays.
	down()
	if p := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/pause", "", unsafe); p.code != http.StatusOK || p.body["status"] != "unknown" {
		t.Fatalf("pause: %d %s", p.code, p.raw)
	}
	if x := read(); x.leased || x.lsStatus != "unknown" || x.msStatus != "unknown" || !x.tls {
		t.Fatalf("after pause: %+v", x)
	}
	if res := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/resume", "", unsafe); res.code != http.StatusOK {
		t.Fatalf("resume: %d %s", res.code, res.raw)
	}

	// URL change with an open incident.
	down()
	pgtest.MustExec(t, admin,
		`INSERT INTO incidents (id, tenant_id, started_at, reason) SELECT '`+incident+`', tenant_id, now() - interval '1 hour', 'timeout' FROM monitors WHERE id = '`+id+`'`,
		`INSERT INTO incident_monitors (tenant_id, incident_id, monitor_id) SELECT tenant_id, '`+incident+`', id FROM monitors WHERE id = '`+id+`'`,
		`UPDATE monitor_state SET current_incident_id = '`+incident+`' WHERE monitor_id = '`+id+`'`)
	// A change of other fields only leaves the runtime state alone.
	if p := c.do(http.MethodPatch, "/api/v1/monitors/"+id, `{"name":"Renamed","url":"https://example.com/a"}`, unsafe); p.code != http.StatusOK {
		t.Fatalf("rename: %d %s", p.code, p.raw)
	}
	if x := read(); !x.leased || x.msStatus != "down" {
		t.Fatalf("rename restarted the monitor: %+v", x)
	}
	p := c.do(http.MethodPatch, "/api/v1/monitors/"+id, `{"url":"https://example.org/b"}`, unsafe)
	if p.code != http.StatusOK || p.body["status"] != "unknown" || p.body["lastErrorKind"] != nil || p.body["tlsExpiresAt"] != nil || p.body["ongoingIncident"] != nil {
		t.Fatalf("url change: %d %s", p.code, p.raw)
	}
	if x := read(); x.leased || x.lsStatus != "unknown" || x.msStatus != "unknown" || x.failures != 0 || x.tls || x.delay > 30*time.Second {
		t.Fatalf("after url change: %+v", x)
	}
	var reason string
	var deliveries int
	if err := admin.QueryRow(ctx, `SELECT resolve_reason, (SELECT count(*) FROM notification_deliveries) FROM incidents WHERE id = $1`, incident).
		Scan(&reason, &deliveries); err != nil || reason != "url_changed" || deliveries != 0 {
		t.Fatalf("incident: reason=%q deliveries=%d err=%v", reason, deliveries, err)
	}
}

// The read endpoints of B-15 through the generated server: shapes, parameter limits and
// tenant isolation (the read models themselves are tested in internal/queries).
func TestIntegrationMonitorReadEndpoints(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apimonread"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()
	admin := env.AdminConn(t)
	r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Site","url":"https://example.com/"}`, withCSRF(cookie, csrf))
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	id := r.body["id"].(string)
	pgtest.MustExec(t, admin,
		`INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, status_code, latency_ms)
		 SELECT tenant_id, id, '01920000-0000-7000-8000-000000000001', now() - interval '1 minute', true, 200, 42 FROM monitors WHERE id = '`+id+`'`,
		`UPDATE monitor_state SET status = 'up', last_checked_at = now() - interval '1 minute' WHERE monitor_id = '`+id+`'`)

	list := c.do(http.MethodGet, "/api/v1/monitors", "", cookie)
	item := list.body["items"].([]any)[0].(map[string]any)
	if item["uptime24h"] != 100.0 || item["lastResponseMs"] != 42.0 || fmt.Sprint(list.body["summary"]) != "map[down:0 paused:0 unknown:0 up:1]" {
		t.Fatalf("list: %s", list.raw)
	}
	if g := c.do(http.MethodGet, "/api/v1/monitors/"+id, "", cookie); fmt.Sprint(g.body["uptime"]) != "map[d30:100 d7:100 h24:100]" {
		t.Fatalf("detail uptime: %s", g.raw)
	}

	st := c.do(http.MethodGet, "/api/v1/monitors/"+id+"/stats", "", cookie)
	series, _ := st.body["series"].([]any)
	if st.code != http.StatusOK || st.body["range"] != "24h" || st.body["avgMs"] != 42.0 || st.body["p95Ms"] != 42.0 || len(series) != 97 {
		t.Fatalf("stats: %d %s", st.code, st.raw)
	}
	if st := c.do(http.MethodGet, "/api/v1/monitors/"+id+"/stats?range=30d", "", cookie); st.code != http.StatusOK || len(st.body["series"].([]any)) != 121 {
		t.Fatalf("stats 30d: %d", st.code)
	}
	ch := c.do(http.MethodGet, "/api/v1/monitors/"+id+"/checks?limit=200", "", cookie)
	items, _ := ch.body["items"].([]any)
	if ch.code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["responseMs"] != 42.0 || items[0].(map[string]any)["location"] != "home-eu-central-1" {
		t.Fatalf("checks: %d %s", ch.code, ch.raw)
	}
	if inc := c.do(http.MethodGet, "/api/v1/monitors/"+id+"/incidents", "", cookie); inc.code != http.StatusOK || !strings.Contains(inc.raw, `"items":[]`) {
		t.Fatalf("incidents: %d %s", inc.code, inc.raw)
	}
	for _, q := range []string{"/stats?range=1y", "/checks?limit=0", "/checks?limit=201"} {
		if r := c.do(http.MethodGet, "/api/v1/monitors/"+id+q, "", cookie); r.code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", q, r.code, r.raw)
		}
	}
	other, _ := otherTenantSession(t, env)
	for _, p := range []string{"", "/stats", "/checks", "/incidents"} {
		if r := c.do(http.MethodGet, "/api/v1/monitors/"+id+p, "", other); r.code != http.StatusNotFound {
			t.Errorf("other tenant %s: %d %s", p, r.code, r.raw)
		}
	}
}
