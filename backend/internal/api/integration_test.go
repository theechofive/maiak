package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const (
	adminEmail    = "admin@example.com"
	adminPassword = "correct horse battery staple"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
}

type response struct {
	code   int
	header http.Header
	body   map[string]any
	raw    string
}

func (c *client) do(method, path, body string, hdr map[string]string) response {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := response{code: resp.StatusCode, header: resp.Header, raw: string(b)}
	_ = json.Unmarshal(b, &r.body)
	return r
}

func sessionCookie(t *testing.T, r response) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		if c.Name == api.SessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", api.SessionCookie)
	return nil
}

func newServer(t *testing.T, env *pgtest.Env, opts auth.Options) *client {
	t.Helper()
	ctx := context.Background()
	pool := env.Pool(t, env.APIURL, 4)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts.SessionTTL, opts.Logger = time.Hour, log
	svc, err := auth.NewService(pool, opts)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := crypto.HashPassword(adminPassword, crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BootstrapAdmin(ctx, adminEmail, hash); err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New()
	h, err := api.NewHandler(api.Config{Version: "test", Commit: "test", CookieSecure: true, TrustedProxyHops: 1},
		api.Deps{Auth: svc, Monitors: monitors.NewService(pool, sched, incidents.NewService(sched)), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

func TestIntegrationLoginMeLogout(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("api"))
	c := newServer(t, env, auth.Options{})
	admin := env.AdminConn(t)
	ctx := context.Background()
	xff := map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.7"}

	// Wrong password and unknown user give the same generic answer.
	for _, body := range []string{
		`{"email":"admin@example.com","password":"wrong"}`,
		`{"email":"nobody@example.com","password":"wrong"}`,
	} {
		r := c.do(http.MethodPost, "/api/v1/auth/login", body, xff)
		if r.code != http.StatusUnauthorized || r.body["type"] != "urn:maiak:problem:invalid-credentials" {
			t.Fatalf("bad login: %d %s", r.code, r.raw)
		}
		if strings.Contains(r.raw, "nobody") {
			t.Fatal("error echoes the email")
		}
	}

	r := c.do(http.MethodPost, "/api/v1/auth/login", `{"email":"Admin@Example.com","password":"`+adminPassword+`"}`, xff)
	if r.code != http.StatusOK {
		t.Fatalf("login: %d %s", r.code, r.raw)
	}
	cookie := sessionCookie(t, r)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge <= 0 {
		t.Fatalf("cookie attributes: %+v", cookie)
	}
	csrf, _ := r.body["csrf_token"].(string)
	if csrf == "" || r.body["role"] != "owner" {
		t.Fatalf("login body: %s", r.raw)
	}
	if tenant, _ := r.body["tenant"].(map[string]any); tenant["slug"] != "default" {
		t.Fatalf("tenant: %v", r.body["tenant"])
	}
	if strings.Contains(r.raw, cookie.Value) {
		t.Fatal("session token must only be in the cookie")
	}

	// Only the SHA-256 of the token is stored.
	var stored int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash = sha256(convert_to($1, 'UTF8'))`, cookie.Value).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored session by hash: %d, %v", stored, err)
	}

	withCookie := func(extra map[string]string) map[string]string {
		h := map[string]string{"Cookie": api.SessionCookie + "=" + cookie.Value}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	me := c.do(http.MethodGet, "/api/v1/auth/me", "", withCookie(nil))
	if me.code != http.StatusOK || me.body["csrf_token"] != csrf {
		t.Fatalf("me: %d %s", me.code, me.raw)
	}
	if user, _ := me.body["user"].(map[string]any); user["email"] != adminEmail {
		t.Fatalf("me user: %v", me.body["user"])
	}

	if r := c.do(http.MethodGet, "/api/v1/auth/me", "", map[string]string{"Cookie": api.SessionCookie + "=garbage"}); r.code != http.StatusUnauthorized {
		t.Fatalf("me with garbage cookie: %d", r.code)
	}

	// Logout needs the CSRF token.
	for _, hdr := range []map[string]string{withCookie(nil), withCookie(map[string]string{"X-CSRF-Token": csrf + "x"})} {
		r := c.do(http.MethodPost, "/api/v1/auth/logout", "", hdr)
		if r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:csrf" {
			t.Fatalf("logout without valid csrf: %d %s", r.code, r.raw)
		}
	}
	out := c.do(http.MethodPost, "/api/v1/auth/logout", "", withCookie(map[string]string{"X-CSRF-Token": csrf, "X-Forwarded-For": "203.0.113.7"}))
	if out.code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", out.code, out.raw)
	}
	if cleared := sessionCookie(t, out); cleared.MaxAge >= 0 && !cleared.Expires.Before(time.Now()) {
		t.Fatalf("logout did not clear the cookie: %+v", cleared)
	}
	if r := c.do(http.MethodGet, "/api/v1/auth/me", "", withCookie(nil)); r.code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", r.code)
	}

	// Audit trail, with the client IP taken from the trusted X-Forwarded-For hop.
	rows, err := admin.Query(ctx, `SELECT action, host(ip), actor_user_id IS NOT NULL FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, ip string
		var hasActor bool
		if err := rows.Scan(&action, &ip, &hasActor); err != nil {
			t.Fatal(err)
		}
		if ip != "203.0.113.7" {
			t.Errorf("%s recorded ip %s", action, ip)
		}
		got = append(got, action+map[bool]string{true: "+actor", false: ""}[hasActor])
	}
	want := "auth.login.failed+actor,auth.login.failed,auth.login.succeeded+actor,auth.logout+actor"
	if strings.Join(got, ",") != want {
		t.Fatalf("audit = %s\nwant    %s", strings.Join(got, ","), want)
	}
}

func TestIntegrationLoginRateLimit(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apilimit"))
	fake := clock.NewFake(time.Now())
	c := newServer(t, env, auth.Options{
		Clock:        fake,
		IPLimiter:    auth.NewLimiter(100, time.Minute, 100, fake),
		EmailLimiter: auth.NewLimiter(2, time.Minute, 100, fake),
	})
	bad := `{"email":"admin@example.com","password":"wrong"}`
	for i := 0; i < 2; i++ {
		if r := c.do(http.MethodPost, "/api/v1/auth/login", bad, nil); r.code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	// Even the right password is refused while the email is limited.
	good := `{"email":"admin@example.com","password":"` + adminPassword + `"}`
	r := c.do(http.MethodPost, "/api/v1/auth/login", good, nil)
	if r.code != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" || r.body["type"] != "urn:maiak:problem:rate-limited" {
		t.Fatalf("limited: %d %v %s", r.code, r.header, r.raw)
	}
	fake.Advance(time.Minute)
	if r := c.do(http.MethodPost, "/api/v1/auth/login", good, nil); r.code != http.StatusOK {
		t.Fatalf("after window: %d %s", r.code, r.raw)
	}
}

// Behind a trusted proxy, a login without a usable X-Forwarded-For is limited under one
// shared key, not under the proxy's address (which every client shares).
func TestIntegrationLoginWithoutForwardedForUsesSharedKey(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apixff"))
	fake := clock.NewFake(time.Now())
	c := newServer(t, env, auth.Options{
		Clock:        fake,
		IPLimiter:    auth.NewLimiter(2, time.Minute, 100, fake),
		EmailLimiter: auth.NewLimiter(100, time.Minute, 100, fake),
	})
	login := func(email string, hdr map[string]string) int {
		return c.do(http.MethodPost, "/api/v1/auth/login", `{"email":"`+email+`","password":"wrong"}`, hdr).code
	}
	if code := login("a@example.com", nil); code != http.StatusUnauthorized {
		t.Fatalf("first: %d", code)
	}
	if code := login("b@example.com", map[string]string{"X-Forwarded-For": "not-an-ip"}); code != http.StatusUnauthorized {
		t.Fatalf("second: %d", code)
	}
	if code := login("c@example.com", nil); code != http.StatusTooManyRequests {
		t.Fatalf("shared bucket not limited: %d", code)
	}
	// A client with a valid forwarded address is unaffected.
	if code := login("d@example.com", map[string]string{"X-Forwarded-For": "198.51.100.1"}); code != http.StatusUnauthorized {
		t.Fatalf("attributed client: %d", code)
	}
	var withIP, withoutIP int
	if err := env.AdminConn(t).QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE ip IS NOT NULL), count(*) FILTER (WHERE ip IS NULL) FROM audit_log`).Scan(&withIP, &withoutIP); err != nil {
		t.Fatal(err)
	}
	if withIP != 1 || withoutIP != 2 {
		t.Fatalf("audit rows with ip=%d without=%d, want 1/2 (the proxy address is not recorded)", withIP, withoutIP)
	}
}

// login returns the session cookie header and CSRF token of the admin.
func (c *client) login() (map[string]string, string) {
	c.t.Helper()
	r := c.do(http.MethodPost, "/api/v1/auth/login", `{"email":"`+adminEmail+`","password":"`+adminPassword+`"}`, nil)
	if r.code != http.StatusOK {
		c.t.Fatalf("login: %d %s", r.code, r.raw)
	}
	csrf, _ := r.body["csrf_token"].(string)
	return map[string]string{"Cookie": api.SessionCookie + "=" + sessionCookie(c.t, r).Value}, csrf
}

// Every unsafe operation of the contract except login refuses a valid session without the
// right CSRF token, before any handler (implemented or not) runs.
func TestIntegrationUnsafeOperationsNeedCSRF(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apicsrf"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()

	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range spec.Paths.InMatchingOrder() {
		for method, op := range spec.Paths.Value(path).Operations() {
			if method == http.MethodGet || strings.EqualFold(op.OperationID, "login") {
				continue
			}
			url := strings.ReplaceAll(path, "{monitorId}", "0192b9c4-7c5e-7000-8000-000000000001")
			for name, token := range map[string]string{"missing": "", "wrong": csrf + "x"} {
				hdr := map[string]string{"Cookie": cookie["Cookie"]}
				if token != "" {
					hdr["X-CSRF-Token"] = token
				}
				r := c.do(method, url, "", hdr)
				if r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:csrf" {
					t.Errorf("%s %s with %s token: %d %s", method, url, name, r.code, r.raw)
				}
			}
			checked++
		}
	}
	if checked < 8 {
		t.Fatalf("only %d unsafe operations checked", checked)
	}
	// The session survived all of it.
	if r := c.do(http.MethodGet, "/api/v1/auth/me", "", cookie); r.code != http.StatusOK {
		t.Fatalf("me: %d %s", r.code, r.raw)
	}
}

// Operations without a handler yet answer 501 problem+json to an authenticated client.
func TestIntegrationUnimplementedOperationIs501(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("api501"))
	c := newServer(t, env, auth.Options{})
	cookie, _ := c.login()
	r := c.do(http.MethodGet, "/api/v1/settings/slack", "", cookie)
	if r.code != http.StatusNotImplemented || r.body["type"] != "urn:maiak:problem:not-implemented" ||
		!strings.HasPrefix(r.header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("slack settings: %d %v %s", r.code, r.header, r.raw)
	}
}
