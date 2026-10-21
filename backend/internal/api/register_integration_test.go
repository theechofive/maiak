package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const ownerPassword = "registered owner passphrase"

// newRegistrationServer wires every service the registration tests touch (auth, monitors,
// queries, Slack settings, plans), logs with the production JSON logger into logs and sets the
// registration kill switch.
func newRegistrationServer(t *testing.T, env *pgtest.Env, opts auth.Options, registration bool, logs *lockedBuffer) *client {
	t.Helper()
	ctx := context.Background()
	pool := env.Pool(t, env.APIURL, 8)
	log := logging.New(logs, slog.LevelDebug)
	opts.SessionTTL, opts.Logger = time.Hour, log
	svc, err := auth.NewService(pool, opts)
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
	h, err := api.NewHandler(api.Config{Version: "test", Commit: "test", CookieSecure: true, TrustedProxyHops: 1, RegistrationEnabled: registration},
		api.Deps{
			Auth: svc, Monitors: monitors.NewService(pool, sched, incidents.NewService(sched)), Queries: queries.New(pool, nil),
			Slack: notify.NewService(pool, keys, &fakeSlack{}, "https://maiak.example.com"), Plans: plans.NewService(pool), Logger: log,
		})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

func registerBody(email, password string) string {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password})
	return string(b)
}

// session returns the cookie header and CSRF token of a 200/201 login or registration answer.
func session(t *testing.T, r response) (map[string]string, string) {
	t.Helper()
	csrf, _ := r.body["csrf_token"].(string)
	if csrf == "" {
		t.Fatalf("no csrf_token: %s", r.raw)
	}
	return map[string]string{"Cookie": api.SessionCookie + "=" + sessionCookie(t, r).Value}, csrf
}

func (c *client) register(email string) (map[string]string, string, response) {
	c.t.Helper()
	r := c.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, ownerPassword), map[string]string{"X-Forwarded-For": "198.51.100.20"})
	if r.code != http.StatusCreated {
		c.t.Fatalf("register %s: %d %s", email, r.code, r.raw)
	}
	cookie, csrf := session(c.t, r)
	return cookie, csrf, r
}

func countRows(t *testing.T, env *pgtest.Env, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := env.AdminConn(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestIntegrationRegister(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiregister"))
	logs := &lockedBuffer{}
	c := newRegistrationServer(t, env, auth.Options{}, true, logs)
	admin := env.AdminConn(t)
	ctx := context.Background()

	if r := c.do(http.MethodGet, "/api/v1/public/config", "", nil); r.code != http.StatusOK || r.body["registrationEnabled"] != true {
		t.Fatalf("public config: %d %s", r.code, r.raw)
	}

	// Trimmed and lowercased; no CSRF header needed; logged in at once.
	cookie, csrf, r := c.register("  Owner+Ops@Example.COM ")
	c2 := sessionCookie(t, r)
	if !c2.HttpOnly || !c2.Secure || c2.SameSite != http.SameSiteLaxMode || c2.MaxAge <= 0 {
		t.Fatalf("cookie attributes: %+v", c2)
	}
	user, _ := r.body["user"].(map[string]any)
	tenant, _ := r.body["tenant"].(map[string]any)
	slug, _ := tenant["slug"].(string)
	if user["login"] != "owner+ops@example.com" || r.body["role"] != "owner" || tenant["name"] != "example.com" ||
		!strings.HasPrefix(slug, "owner-ops-") || len(slug) != len("owner-ops-")+6 {
		t.Fatalf("register body: %s", r.raw)
	}
	tenantID, _ := tenant["id"].(string)
	userID, _ := user["id"].(string)

	// Rows: the tenant on free, the user with an argon2id hash, the owner membership, one session.
	var plan, email, hash, role string
	if err := admin.QueryRow(ctx, `SELECT t.plan_key, u.email::text, u.password_hash, m.role FROM tenants t
		JOIN memberships m ON m.tenant_id = t.id JOIN users u ON u.id = m.user_id WHERE t.id = $1 AND u.id = $2`, tenantID, userID).
		Scan(&plan, &email, &hash, &role); err != nil {
		t.Fatal(err)
	}
	if plan != "free" || email != "owner+ops@example.com" || !strings.HasPrefix(hash, "$argon2id$") || role != "owner" {
		t.Fatalf("rows: plan=%s email=%s hash=%.10s role=%s", plan, email, hash, role)
	}
	if n := countRows(t, env, `SELECT count(*) FROM sessions WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
	// One audit row, with only the tenant id as metadata.
	var action, actor, entity, meta, ip string
	if err := admin.QueryRow(ctx, `SELECT action, actor_user_id::text, entity_id, metadata::text, host(ip) FROM audit_log WHERE tenant_id = $1`, tenantID).
		Scan(&action, &actor, &entity, &meta, &ip); err != nil {
		t.Fatal(err)
	}
	if action != "auth.register" || actor != userID || entity != userID || meta != fmt.Sprintf(`{"tenant_id": "%s"}`, tenantID) || ip != "198.51.100.20" {
		t.Fatalf("audit: %s %s %s %s %s", action, actor, entity, meta, ip)
	}

	// The session works: me, usage on the free plan, an empty monitor list, a monitor create.
	if me := c.do(http.MethodGet, "/api/v1/auth/me", "", cookie); me.code != http.StatusOK || me.body["csrf_token"] != csrf ||
		!strings.Contains(me.raw, `"login":"owner+ops@example.com"`) {
		t.Fatalf("me: %d %s", me.code, me.raw)
	}
	if u := c.do(http.MethodGet, "/api/v1/usage", "", cookie); u.code != http.StatusOK ||
		u.raw != `{"limits":{"channels":1,"heartbeats":1,"monitors":3},"plan":{"key":"free","name":"Free"},"usage":{"channels":0,"heartbeats":0,"monitors":0}}`+"\n" {
		t.Fatalf("usage: %d %s", u.code, u.raw)
	}
	if l := c.do(http.MethodGet, "/api/v1/monitors", "", cookie); l.code != http.StatusOK || !strings.Contains(l.raw, `"items":[]`) {
		t.Fatalf("list: %d %s", l.code, l.raw)
	}
	if m := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Site","url":"https://example.com"}`, withCSRF(cookie, csrf)); m.code != http.StatusCreated {
		t.Fatalf("create: %d %s", m.code, m.raw)
	}

	// Login by email, case-insensitive, binds to the registered tenant; the admin still binds to default.
	l := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"OWNER+ops@example.com","password":"`+ownerPassword+`"}`, nil)
	if l.code != http.StatusOK || !strings.Contains(l.raw, `"id":"`+tenantID+`"`) || l.body["role"] != "owner" {
		t.Fatalf("login by email: %d %s", l.code, l.raw)
	}
	a := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"admin","password":"`+adminPassword+`"}`, nil)
	if at, _ := a.body["tenant"].(map[string]any); a.code != http.StatusOK || at["slug"] != "default" {
		t.Fatalf("admin login: %d %s", a.code, a.raw)
	}
	// Failures stay generic: wrong password, unknown email, a 2-character login.
	for _, body := range []string{
		`{"login":"owner+ops@example.com","password":"wrong password here"}`,
		`{"login":"nobody@example.com","password":"` + ownerPassword + `"}`,
		`{"login":"ab","password":"` + ownerPassword + `"}`,
	} {
		if f := c.do(http.MethodPost, "/api/v1/auth/login", body, nil); f.code != http.StatusUnauthorized ||
			f.body["type"] != "urn:maiak:problem:invalid-credentials" || f.body["title"] != "Wrong login or password" {
			t.Fatalf("failed login %s: %d %s", body, f.code, f.raw)
		}
	}
	// The failed login of a registered user is audited in that user's tenant.
	if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'auth.login.failed' AND actor_user_id = $2`, tenantID, userID); n != 1 {
		t.Fatalf("failed login audit rows in the user's tenant = %d", n)
	}

	// The same email again, in another case: 409, nothing created.
	tenantsBefore := countRows(t, env, `SELECT count(*) FROM tenants`)
	d := c.do(http.MethodPost, "/api/v1/auth/register", registerBody("OWNER+OPS@example.com", "another long password"), nil)
	if d.code != http.StatusConflict || d.body["type"] != "urn:maiak:problem:email-taken" || strings.Contains(d.raw, "owner") {
		t.Fatalf("duplicate: %d %s", d.code, d.raw)
	}
	// The system login cannot be registered (not an email), nor can a 2-character one.
	for _, e := range []string{"admin", "ab"} {
		if r := c.do(http.MethodPost, "/api/v1/auth/register", registerBody(e, "another long password"), nil); r.code != http.StatusUnprocessableEntity {
			t.Fatalf("register %q: %d %s", e, r.code, r.raw)
		}
	}
	if n := countRows(t, env, `SELECT count(*) FROM tenants`); n != tenantsBefore {
		t.Fatalf("tenants %d -> %d after rejected registrations", tenantsBefore, n)
	}

	// Neither the password nor its hash nor the session token reaches the logs or the audit log.
	out := logs.String()
	for _, secret := range []string{ownerPassword, hash, c2.Value, csrf, "another long password", "wrong password here"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain a secret (%.12s...)", secret)
		}
	}
	if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE metadata::text ~* 'password|argon2|@'`); n != 0 {
		t.Fatalf("%d audit rows carry a password, hash or email", n)
	}
	if !strings.Contains(out, `"msg":"account registered"`) {
		t.Fatalf("registration not logged:\n%s", out)
	}
}

func TestIntegrationRegisterPolicy(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiregpolicy"))
	c := newRegistrationServer(t, env, auth.Options{IPLimiter: auth.NewLimiter(1000, time.Minute, 100, clock.System{})}, true, &lockedBuffer{})
	for name, tc := range map[string]struct{ email, password, field string }{
		"11 characters":       {"a@example.com", "elevenchars", "password"},
		"129 characters":      {"a@example.com", strings.Repeat("p", 129), "password"},
		"11 code points":      {"a@example.com", strings.Repeat("ж", 11), "password"},
		"equals the email":    {"Same.Address@example.com", "same.address@EXAMPLE.com", "password"},
		"unicode email":       {"jörg@example.com", ownerPassword, "email"},
		"space inside":        {"a b@example.com", ownerPassword, "email"},
		"no domain dot":       {"a@localhost", ownerPassword, "email"},
		"two @":               {"a@b@example.com", ownerPassword, "email"},
		"control character":   {"a\t@example.com", ownerPassword, "email"},
		"255 characters":      {strings.Repeat("a", 243) + "@example.com", ownerPassword, "email"},
		"quote in local part": {"o'brien@example.com", ownerPassword, "email"},
	} {
		r := c.do(http.MethodPost, "/api/v1/auth/register", registerBody(tc.email, tc.password), nil)
		fe, _ := r.body["fieldErrors"].([]any)
		first, _ := func() (map[string]any, bool) {
			if len(fe) == 0 {
				return nil, false
			}
			m, ok := fe[0].(map[string]any)
			return m, ok
		}()
		if r.code != http.StatusUnprocessableEntity || r.body["type"] != "urn:maiak:problem:validation" || first["field"] != tc.field {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
		if strings.Contains(r.raw, tc.password) {
			t.Errorf("%s: answer echoes the password", name)
		}
	}
	// Edge cases that pass: 12 and 128 code points, multi-byte passwords, surrounding whitespace in the email.
	for i, tc := range []struct{ email, password string }{
		{" edge1@example.com\t", strings.Repeat("ж", 12)},
		{"edge2@example.com", strings.Repeat("p", 128)},
		{"Edge3@Sub.Example.co.uk", "пароль-пароль-пароль"},
	} {
		if r := c.do(http.MethodPost, "/api/v1/auth/register", registerBody(tc.email, tc.password), nil); r.code != http.StatusCreated {
			t.Errorf("edge case %d: %d %s", i, r.code, r.raw)
		}
	}
	if n := countRows(t, env, `SELECT count(*) FROM tenants WHERE plan_key = 'free'`); n != 3 {
		t.Fatalf("free tenants = %d, want 3", n)
	}
}

// Concurrent registrations of one email: exactly one 201, the others 409, and the rolled-back
// transactions leave no tenant, user or membership behind.
func TestIntegrationRegisterConcurrentDuplicate(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiregrace"))
	c := newRegistrationServer(t, env, auth.Options{}, true, &lockedBuffer{})
	const n = 4
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			codes[i] = c.do(http.MethodPost, "/api/v1/auth/register", registerBody("race@example.com", ownerPassword),
				map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i+1)}).code
		})
	}
	wg.Wait()
	created, conflicts := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		}
	}
	if created != 1 || conflicts != n-1 {
		t.Fatalf("codes = %v, want one 201 and %d 409", codes, n-1)
	}
	if got := countRows(t, env, `SELECT count(*) FROM tenants WHERE slug <> 'default'`); got != 1 {
		t.Fatalf("tenants besides default = %d, want 1 (no orphans)", got)
	}
	if got := countRows(t, env, `SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id WHERE u.email = 'race@example.com'`); got != 1 {
		t.Fatalf("memberships = %d", got)
	}
}

func TestIntegrationRegistrationKillSwitch(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiregoff"))
	c := newRegistrationServer(t, env, auth.Options{}, false, &lockedBuffer{})
	if r := c.do(http.MethodGet, "/api/v1/public/config", "", nil); r.code != http.StatusOK || r.body["registrationEnabled"] != false {
		t.Fatalf("public config: %d %s", r.code, r.raw)
	}
	r := c.do(http.MethodPost, "/api/v1/auth/register", registerBody("off@example.com", ownerPassword), nil)
	if r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:registration-disabled" {
		t.Fatalf("register while disabled: %d %s", r.code, r.raw)
	}
	if n := countRows(t, env, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users = %d, want only the admin", n)
	}
	// Login keeps working.
	if l := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"admin","password":"`+adminPassword+`"}`, nil); l.code != http.StatusOK {
		t.Fatalf("login: %d %s", l.code, l.raw)
	}
}

// Every registration attempt that reaches the hash counts against the client address (the
// login's per-IP limiter), a success included; policy failures do not.
func TestIntegrationRegisterRateLimit(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apireglimit"))
	fake := clock.NewFake(time.Now())
	c := newRegistrationServer(t, env, auth.Options{Clock: fake, IPLimiter: auth.NewLimiter(2, time.Minute, 100, fake)}, true, &lockedBuffer{})
	xff := map[string]string{"X-Forwarded-For": "203.0.113.9"}
	reg := func(email, password string) response {
		return c.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, password), xff)
	}
	if r := reg("bad", "short"); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid: %d", r.code)
	}
	if r := reg("one@example.com", ownerPassword); r.code != http.StatusCreated {
		t.Fatalf("first: %d %s", r.code, r.raw)
	}
	if r := reg("one@example.com", ownerPassword); r.code != http.StatusConflict {
		t.Fatalf("duplicate: %d %s", r.code, r.raw)
	}
	r := reg("two@example.com", ownerPassword)
	if r.code != http.StatusTooManyRequests || r.body["type"] != "urn:maiak:problem:rate-limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("third: %d %s", r.code, r.raw)
	}
	// Another client is unaffected; the limited one may register after the window.
	if r := c.do(http.MethodPost, "/api/v1/auth/register", registerBody("three@example.com", ownerPassword), map[string]string{"X-Forwarded-For": "203.0.113.10"}); r.code != http.StatusCreated {
		t.Fatalf("other client: %d %s", r.code, r.raw)
	}
	fake.Advance(time.Minute)
	if r := reg("two@example.com", ownerPassword); r.code != http.StatusCreated {
		t.Fatalf("after window: %d %s", r.code, r.raw)
	}
}

// Two registered tenants see nothing of each other: monitors, Slack settings and usage.
func TestIntegrationRegisteredTenantsAreIsolated(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiregiso"))
	c := newRegistrationServer(t, env, auth.Options{}, true, &lockedBuffer{})
	aCookie, aCSRF, _ := c.register("alice@a.example.com")
	bCookie, bCSRF, _ := c.register("bob@b.example.com")

	m := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Alice site","url":"https://a.example.com"}`, withCSRF(aCookie, aCSRF))
	if m.code != http.StatusCreated {
		t.Fatalf("alice create: %d %s", m.code, m.raw)
	}
	id := m.body["id"].(string)
	if s := c.do(http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"`+slackURL+`"}`, withCSRF(aCookie, aCSRF)); s.code != http.StatusOK {
		t.Fatalf("alice slack: %d %s", s.code, s.raw)
	}

	if l := c.do(http.MethodGet, "/api/v1/monitors", "", bCookie); l.code != http.StatusOK || !strings.Contains(l.raw, `"items":[]`) {
		t.Fatalf("bob list: %d %s", l.code, l.raw)
	}
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/monitors/" + id, ""},
		{http.MethodGet, "/api/v1/monitors/" + id + "/checks", ""},
		{http.MethodPatch, "/api/v1/monitors/" + id, `{"name":"Hijacked"}`},
		{http.MethodPost, "/api/v1/monitors/" + id + "/pause", ""},
		{http.MethodDelete, "/api/v1/monitors/" + id, ""},
	} {
		if r := c.do(rq.method, rq.path, rq.body, withCSRF(bCookie, bCSRF)); r.code != http.StatusNotFound {
			t.Errorf("bob %s %s: %d %s", rq.method, rq.path, r.code, r.raw)
		}
	}
	if s := c.do(http.MethodGet, "/api/v1/settings/slack", "", bCookie); s.code != http.StatusOK || s.body["configured"] != false {
		t.Fatalf("bob slack: %d %s", s.code, s.raw)
	}
	usage := func(cookie map[string]string) string {
		u := c.do(http.MethodGet, "/api/v1/usage", "", cookie)
		if u.code != http.StatusOK {
			t.Fatalf("usage: %d %s", u.code, u.raw)
		}
		b, _ := json.Marshal(u.body["usage"])
		return string(b)
	}
	if got := usage(bCookie); got != `{"channels":0,"heartbeats":0,"monitors":0}` {
		t.Fatalf("bob usage = %s", got)
	}
	if got := usage(aCookie); got != `{"channels":1,"heartbeats":0,"monitors":1}` {
		t.Fatalf("alice usage = %s", got)
	}
	if g := c.do(http.MethodGet, "/api/v1/monitors/"+id, "", aCookie); g.code != http.StatusOK || g.body["name"] != "Alice site" {
		t.Fatalf("alice's monitor changed: %d %s", g.code, g.raw)
	}
}
