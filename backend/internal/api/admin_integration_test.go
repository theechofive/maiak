package api_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/admin"
	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// newAdminServer wires the admin panel next to registration, monitors and heartbeats.
func newAdminServer(t *testing.T, env *pgtest.Env, logs *lockedBuffer) *client {
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
	adminSvc, err := admin.NewService(pool, svc, admin.Options{Logger: log, Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New()
	h, err := api.NewHandler(api.Config{Version: "test", Commit: "test", CookieSecure: true, TrustedProxyHops: 1, RegistrationEnabled: true, PublicURL: heartbeatPublicURL},
		api.Deps{Auth: svc, Monitors: monitors.NewService(pool, sched, incidents.NewService(sched)), Queries: queries.New(pool, nil),
			Plans: plans.NewService(pool), Admin: adminSvc, Logger: log, Pings: api.NewPingMetrics(prometheus.NewRegistry())})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

// adminCalls are one concrete request per admin operation of the contract.
func adminCalls(t *testing.T, tenantID, userID string) []struct{ method, path, body string } {
	t.Helper()
	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	var out []struct{ method, path, body string }
	for _, path := range spec.Paths.InMatchingOrder() {
		if !strings.HasPrefix(path, "/api/v1/admin/") {
			continue
		}
		for method := range spec.Paths.Value(path).Operations() {
			body := ""
			if method == http.MethodPatch {
				body = `{"status":"suspended"}`
			}
			out = append(out, struct{ method, path, body string }{method,
				strings.NewReplacer("{tenantId}", tenantID, "{userId}", userID).Replace(path), body})
		}
	}
	if len(out) != 7 {
		t.Fatalf("%d admin operations in the spec, want 7", len(out))
	}
	return out
}

// Every admin operation answers 401 without a session and 403 to a registered user (with a valid
// CSRF token), changes nothing, and the admin's own answers work.
func TestIntegrationAdminOperationsRefuseNonAdmins(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiadminguard"))
	logs := &lockedBuffer{}
	c := newAdminServer(t, env, logs)
	userCookie, userCSRF, reg := c.register("owner@tenant.example")
	tenant := reg.body["tenant"].(map[string]any)["id"].(string)
	user := reg.body["user"].(map[string]any)["id"].(string)
	if v, ok := reg.body["user"].(map[string]any)["isPlatformAdmin"]; !ok || v != false {
		t.Fatalf("registered user isPlatformAdmin = %v (present %v)", v, ok)
	}
	for _, call := range adminCalls(t, tenant, user) {
		if r := c.do(call.method, call.path, call.body, map[string]string{"X-CSRF-Token": "x"}); r.code != http.StatusUnauthorized {
			t.Errorf("%s %s without session: %d %s", call.method, call.path, r.code, r.raw)
		}
		r := c.do(call.method, call.path, call.body, withCSRF(userCookie, userCSRF))
		if r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:forbidden" {
			t.Errorf("%s %s as a registered user: %d %s", call.method, call.path, r.code, r.raw)
		}
	}
	if n := countRows(t, env, `SELECT count(*) FROM tenants WHERE status <> 'active' OR deleted_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d tenants changed by refused calls", n)
	}
	if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE action LIKE 'admin.%'`); n != 0 {
		t.Fatalf("%d admin audit rows after refused calls", n)
	}
	// The registered user's session still works and its own data is untouched.
	if r := c.do(http.MethodGet, "/api/v1/monitors", "", userCookie); r.code != http.StatusOK {
		t.Fatalf("user session after refusals: %d", r.code)
	}

	adminCookie, adminCSRF := c.login()
	me := c.do(http.MethodGet, "/api/v1/auth/me", "", adminCookie)
	if me.body["user"].(map[string]any)["isPlatformAdmin"] != true {
		t.Fatalf("admin me: %s", me.raw)
	}
	// A CSRF token is still required from the admin (no exemption).
	if r := c.do(http.MethodDelete, "/api/v1/admin/tenants/"+tenant, "", adminCookie); r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:csrf" {
		t.Fatalf("admin delete without csrf: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodGet, "/api/v1/admin/stats", "", withCSRF(adminCookie, adminCSRF)); r.code != http.StatusOK {
		t.Fatalf("admin stats: %d %s", r.code, r.raw)
	}
}

func TestIntegrationAdminPanel(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiadmin"))
	logs := &lockedBuffer{}
	c := newAdminServer(t, env, logs)
	ctx := context.Background()
	adminCookie, adminCSRF := c.login()
	ah := withCSRF(adminCookie, adminCSRF)

	// Three registered tenants; the first gets an http monitor and a heartbeat.
	userCookie, userCSRF, reg := c.register("alice@alpha.example")
	tenantA := reg.body["tenant"].(map[string]any)["id"].(string)
	userA := reg.body["user"].(map[string]any)["id"].(string)
	_, _, regB := c.register("bob@beta.example")
	tenantB := regB.body["tenant"].(map[string]any)["id"].(string)
	c.register("carol@gamma.example")
	uh := withCSRF(userCookie, userCSRF)
	if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Site","url":"https://example.com"}`, uh); r.code != http.StatusCreated {
		t.Fatalf("create monitor: %d %s", r.code, r.raw)
	}
	hb := c.do(http.MethodPost, "/api/v1/monitors", `{"type":"heartbeat","name":"Watchdog"}`, uh)
	if hb.code != http.StatusCreated {
		t.Fatalf("create heartbeat: %d %s", hb.code, hb.raw)
	}
	pingPath, _ := tokenOf(t, hb.body["heartbeatUrl"])

	t.Run("stats", func(t *testing.T) {
		r := c.do(http.MethodGet, "/api/v1/admin/stats", "", ah)
		if r.code != http.StatusOK || fmt.Sprint(r.body["tenants"]) != "map[active:4 deleted:0 suspended:0 total:4]" ||
			r.body["users"] != float64(4) || r.body["monitors"] != float64(1) || r.body["heartbeats"] != float64(1) {
			t.Fatalf("stats: %d %s", r.code, r.raw)
		}
		if !strings.Contains(r.raw, `{"key":"free","name":"Free","tenants":3}`) || !strings.Contains(r.raw, `{"key":"internal","name":"Internal","tenants":1}`) {
			t.Fatalf("stats plans: %s", r.raw)
		}
	})

	t.Run("list, search and pagination", func(t *testing.T) {
		r := c.do(http.MethodGet, "/api/v1/admin/tenants", "", ah)
		items, _ := r.body["items"].([]any)
		if r.code != http.StatusOK || len(items) != 4 || r.body["total"] != float64(4) || r.body["limit"] != float64(25) || r.body["offset"] != float64(0) {
			t.Fatalf("list: %d %s", r.code, r.raw)
		}
		first := items[0].(map[string]any)
		if first["name"] != "gamma.example" || first["planKey"] != "free" || first["status"] != "active" || first["deletedAt"] != nil {
			t.Fatalf("newest first: %v", first)
		}
		r = c.do(http.MethodGet, "/api/v1/admin/tenants?limit=2&offset=2", "", ah)
		items, _ = r.body["items"].([]any)
		if len(items) != 2 || r.body["total"] != float64(4) || items[1].(map[string]any)["slug"] != "default" {
			t.Fatalf("page 2: %s", r.raw)
		}
		r = c.do(http.MethodGet, "/api/v1/admin/tenants?offset=40", "", ah)
		if items, _ := r.body["items"].([]any); len(items) != 0 || r.body["total"] != float64(4) {
			t.Fatalf("past the end: %s", r.raw)
		}
		r = c.do(http.MethodGet, "/api/v1/admin/tenants?query=ALICE%40alpha", "", ah)
		items, _ = r.body["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["id"] != tenantA {
			t.Fatalf("search by member login: %s", r.raw)
		}
		a := items[0].(map[string]any)
		if a["users"] != float64(1) || a["monitors"] != float64(1) || a["heartbeats"] != float64(1) {
			t.Fatalf("usage of A: %v", a)
		}
		for _, q := range []string{"limit=0", "limit=101", "offset=-1", "includeDeleted=maybe", "query=" + strings.Repeat("x", 201)} {
			if r := c.do(http.MethodGet, "/api/v1/admin/tenants?"+q, "", ah); r.code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", q, r.code, r.raw)
			}
		}
		if r := c.do(http.MethodGet, "/api/v1/admin/tenants/not-a-uuid", "", ah); r.code != http.StatusBadRequest {
			t.Errorf("bad id: %d", r.code)
		}
		if r := c.do(http.MethodGet, "/api/v1/admin/tenants/0192b9c4-7c5e-7000-8000-000000000999", "", ah); r.code != http.StatusNotFound {
			t.Errorf("unknown tenant: %d %s", r.code, r.raw)
		}
	})

	t.Run("validation and guard rails", func(t *testing.T) {
		for body, field := range map[string]string{
			`{}`:                     "planKey",
			`{"planKey":"gold"}`:     "planKey",
			`{"status":"paused"}`:    "status",
			`{"status":"SUSPENDED"}`: "status",
		} {
			r := c.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantA, body, ah)
			fe, _ := r.body["fieldErrors"].([]any)
			if r.code != http.StatusUnprocessableEntity || len(fe) == 0 || fe[0].(map[string]any)["field"] != field {
				t.Errorf("%s: %d %s", body, r.code, r.raw)
			}
		}
		if r := c.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantA, `{"plan":"free"}`, ah); r.code != http.StatusBadRequest {
			t.Errorf("unknown field: %d", r.code)
		}
		var defaultID string
		if err := env.AdminConn(t).QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = 'default'`).Scan(&defaultID); err != nil {
			t.Fatal(err)
		}
		for _, rq := range []struct{ method, body string }{{http.MethodPatch, `{"status":"suspended"}`}, {http.MethodDelete, ""}} {
			r := c.do(rq.method, "/api/v1/admin/tenants/"+defaultID, rq.body, ah)
			if r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:protected-tenant" {
				t.Errorf("%s default: %d %s", rq.method, r.code, r.raw)
			}
		}
		// The system admin's password is managed by its Secret.
		var adminID string
		if err := env.AdminConn(t).QueryRow(ctx, `SELECT id::text FROM users WHERE email = 'admin'`).Scan(&adminID); err != nil {
			t.Fatal(err)
		}
		r := c.do(http.MethodPost, "/api/v1/admin/users/"+adminID+"/reset-password", "", ah)
		if r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:system-admin" || !strings.Contains(fmt.Sprint(r.body["detail"]), "Secret") {
			t.Fatalf("reset of the system admin: %d %s", r.code, r.raw)
		}
		if r := c.do(http.MethodPost, "/api/v1/admin/users/0192b9c4-7c5e-7000-8000-000000000999/reset-password", "", ah); r.code != http.StatusNotFound {
			t.Fatalf("reset of an unknown user: %d %s", r.code, r.raw)
		}
	})

	t.Run("password reset", func(t *testing.T) {
		r := c.do(http.MethodGet, "/api/v1/admin/tenants/"+tenantA+"/users", "", ah)
		items, _ := r.body["items"].([]any)
		if r.code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["login"] != "alice@alpha.example" ||
			items[0].(map[string]any)["role"] != "owner" || items[0].(map[string]any)["isPlatformAdmin"] != false {
			t.Fatalf("users: %d %s", r.code, r.raw)
		}
		r = c.do(http.MethodPost, "/api/v1/admin/users/"+userA+"/reset-password", "", ah)
		password, _ := r.body["temporaryPassword"].(string)
		if r.code != http.StatusOK || len(password) < 20 {
			t.Fatalf("reset: %d %s", r.code, r.raw)
		}
		if r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q", r.header.Get("Cache-Control"))
		}
		// The user's old session is gone, the old password fails, the temporary one works.
		if r := c.do(http.MethodGet, "/api/v1/monitors", "", userCookie); r.code != http.StatusUnauthorized {
			t.Fatalf("old session after reset: %d", r.code)
		}
		if r := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"alice@alpha.example","password":"`+ownerPassword+`"}`, nil); r.code != http.StatusUnauthorized {
			t.Fatalf("old password after reset: %d", r.code)
		}
		login := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"alice@alpha.example","password":"`+password+`"}`, nil)
		if login.code != http.StatusOK {
			t.Fatalf("temporary password login: %d %s", login.code, login.raw)
		}
		userCookie, userCSRF = session(t, login)
		uh = withCSRF(userCookie, userCSRF)
		// The temporary password satisfies the registration policy (it could be registered).
		if r := c.do(http.MethodPost, "/api/v1/auth/register", registerBody("dave@delta.example", password), map[string]string{"X-Forwarded-For": "198.51.100.30"}); r.code != http.StatusCreated {
			t.Fatalf("temporary password against the policy: %d %s", r.code, r.raw)
		}
		if strings.Contains(logs.String(), password) {
			t.Fatal("the temporary password is in the logs")
		}
		var leaks int
		if err := env.AdminConn(t).QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE metadata::text LIKE '%' || $1 || '%' OR metadata::text LIKE '%argon2%'`, password).Scan(&leaks); err != nil || leaks != 0 {
			t.Fatalf("audit rows with the password or a hash: %d %v", leaks, err)
		}
		if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE action = 'admin.user.password_reset' AND tenant_id = $1 AND host(ip) IS NOT NULL`, tenantA); n != 1 {
			t.Fatalf("password reset audit rows: %d", n)
		}
	})

	t.Run("plan downgrade keeps data and blocks only new creations", func(t *testing.T) {
		r := c.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantA, `{"planKey":"internal"}`, ah)
		if r.code != http.StatusOK || r.body["planKey"] != "internal" || r.body["planName"] != "Internal" {
			t.Fatalf("upgrade: %d %s", r.code, r.raw)
		}
		for i := range 3 {
			if r := c.do(http.MethodPost, "/api/v1/monitors", fmt.Sprintf(`{"name":"m%d","url":"https://example.com/%d"}`, i, i), uh); r.code != http.StatusCreated {
				t.Fatalf("create on internal: %d %s", r.code, r.raw)
			}
		}
		if r := c.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantA, `{"planKey":"free"}`, ah); r.code != http.StatusOK || r.body["monitors"] != float64(4) {
			t.Fatalf("downgrade: %d %s", r.code, r.raw)
		}
		u := c.do(http.MethodGet, "/api/v1/usage", "", userCookie)
		if fmt.Sprint(u.body["usage"]) != "map[channels:0 heartbeats:1 monitors:4]" || fmt.Sprint(u.body["limits"]) != "map[channels:1 heartbeats:1 monitors:3]" {
			t.Fatalf("usage over the limit: %s", u.raw)
		}
		if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"one more","url":"https://example.com/x"}`, uh); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:plan-limit-reached" {
			t.Fatalf("create above the new limit: %d %s", r.code, r.raw)
		}
		// Existing monitors keep working: editing one is allowed.
		l := c.do(http.MethodGet, "/api/v1/monitors", "", userCookie)
		id := l.body["items"].([]any)[0].(map[string]any)["id"].(string)
		if r := c.do(http.MethodPatch, "/api/v1/monitors/"+id, `{"name":"renamed"}`, uh); r.code != http.StatusOK {
			t.Fatalf("edit above the limit: %d %s", r.code, r.raw)
		}
		if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE action = 'admin.tenant.plan_changed' AND tenant_id = $1`, tenantA); n != 2 {
			t.Fatalf("plan audit rows: %d", n)
		}
	})

	t.Run("a plan change waits for a quota check holding the tenant row", func(t *testing.T) {
		admin := env.AdminConn(t)
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		// What plans.CheckCreate does in a create transaction.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantB); err != nil {
			t.Fatal(err)
		}
		done := make(chan response, 1)
		go func() { done <- c.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantB, `{"planKey":"internal"}`, ah) }()
		select {
		case r := <-done:
			t.Fatalf("plan change did not wait for the row lock: %d %s", r.code, r.raw)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if r := <-done; r.code != http.StatusOK || r.body["planKey"] != "internal" {
			t.Fatalf("plan change after the lock: %d %s", r.code, r.raw)
		}
	})

	t.Run("suspension", func(t *testing.T) {
		r := c.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantA, `{"status":"suspended"}`, ah)
		if r.code != http.StatusOK || r.body["status"] != "suspended" {
			t.Fatalf("suspend: %d %s", r.code, r.raw)
		}
		// The existing session is refused, a wrong password is the generic failure, the right
		// password is told about the suspension.
		if r := c.do(http.MethodGet, "/api/v1/monitors", "", userCookie); r.code != http.StatusUnauthorized {
			t.Fatalf("session of a suspended tenant: %d", r.code)
		}
		if r := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"alice@alpha.example","password":"wrong password here"}`, nil); r.code != http.StatusUnauthorized ||
			r.body["type"] != "urn:maiak:problem:invalid-credentials" {
			t.Fatalf("wrong password, suspended tenant: %d %s", r.code, r.raw)
		}
		// The ping of its heartbeat is answered like a paused one: 204, nothing recorded.
		if r := c.do(http.MethodGet, pingPath, "", nil); r.code != http.StatusNoContent {
			t.Fatalf("ping of a suspended tenant: %d %s", r.code, r.raw)
		}
		if n := countRows(t, env, `SELECT count(*) FROM heartbeat_tokens WHERE tenant_id = $1 AND last_ping_at IS NOT NULL`, tenantA); n != 0 {
			t.Fatalf("ping recorded for a suspended tenant")
		}
		r = c.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantA, `{"status":"active"}`, ah)
		if r.code != http.StatusOK || r.body["status"] != "active" {
			t.Fatalf("unsuspend: %d %s", r.code, r.raw)
		}
		time.Sleep(1100 * time.Millisecond) // one ping per token per second
		if r := c.do(http.MethodGet, pingPath, "", nil); r.code != http.StatusNoContent {
			t.Fatalf("ping after unsuspend: %d", r.code)
		}
		if n := countRows(t, env, `SELECT count(*) FROM heartbeat_tokens WHERE tenant_id = $1 AND last_ping_at IS NOT NULL`, tenantA); n != 1 {
			t.Fatalf("ping not recorded after unsuspend")
		}
		if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action IN ('admin.tenant.suspended', 'admin.tenant.unsuspended')`, tenantA); n != 2 {
			t.Fatalf("suspension audit rows: %d", n)
		}
	})

	t.Run("soft delete", func(t *testing.T) {
		if r := c.do(http.MethodDelete, "/api/v1/admin/tenants/"+tenantA, "", ah); r.code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", r.code, r.raw)
		}
		if r := c.do(http.MethodDelete, "/api/v1/admin/tenants/"+tenantA, "", ah); r.code != http.StatusNoContent {
			t.Fatalf("delete again: %d %s", r.code, r.raw)
		}
		if r := c.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantA, `{"planKey":"internal"}`, ah); r.code != http.StatusConflict ||
			r.body["type"] != "urn:maiak:problem:tenant-deleted" {
			t.Fatalf("patch of a deleted tenant: %d %s", r.code, r.raw)
		}
		g := c.do(http.MethodGet, "/api/v1/admin/tenants/"+tenantA, "", ah)
		if g.code != http.StatusOK || g.body["deletedAt"] == nil || g.body["status"] != "suspended" || g.body["monitors"] != float64(4) {
			t.Fatalf("deleted tenant: %d %s", g.code, g.raw)
		}
		if r := c.do(http.MethodGet, "/api/v1/admin/tenants?query=alpha", "", ah); r.body["total"] != float64(0) {
			t.Fatalf("deleted tenant listed by default: %s", r.raw)
		}
		if r := c.do(http.MethodGet, "/api/v1/admin/tenants?query=alpha&includeDeleted=true", "", ah); r.body["total"] != float64(1) {
			t.Fatalf("deleted tenant with includeDeleted: %s", r.raw)
		}
		if r := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"alice@alpha.example","password":"wrong password here"}`, nil); r.code != http.StatusUnauthorized {
			t.Fatalf("wrong password, deleted tenant: %d %s", r.code, r.raw)
		}
		var password string
		r := c.do(http.MethodPost, "/api/v1/admin/users/"+userA+"/reset-password", "", ah)
		password, _ = r.body["temporaryPassword"].(string)
		if r.code != http.StatusOK {
			t.Fatalf("reset in a deleted tenant: %d %s", r.code, r.raw)
		}
		l := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"alice@alpha.example","password":"`+password+`"}`, nil)
		if l.code != http.StatusForbidden || l.body["type"] != "urn:maiak:problem:account-suspended" {
			t.Fatalf("right password, deleted tenant: %d %s", l.code, l.raw)
		}
		if n := countRows(t, env, `SELECT count(*) FROM monitors WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantA); n != 5 {
			t.Fatalf("monitors of a deleted tenant: %d, want all 5 kept", n)
		}
	})

	// Other tenants are unaffected by all of it.
	if r := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"bob@beta.example","password":"`+ownerPassword+`"}`, nil); r.code != http.StatusOK {
		t.Fatalf("other tenant login: %d %s", r.code, r.raw)
	}
}
