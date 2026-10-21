package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationUsage covers GET /api/v1/usage and the monitor quota of P-01: plan limits,
// per-tenant overrides, deleted monitors freeing a slot, and tenant isolation of the counts.
func TestIntegrationUsage(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apiusage"))
	c := newServer(t, env, auth.Options{})
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	admin := env.AdminConn(t)

	usage := func(hdr map[string]string) string {
		t.Helper()
		r := c.do(http.MethodGet, "/api/v1/usage", "", hdr)
		if r.code != http.StatusOK {
			t.Fatalf("usage: %d %s", r.code, r.raw)
		}
		return r.raw
	}
	if got, want := usage(cookie), `{"limits":{"channels":10,"heartbeats":100,"monitors":1000},"plan":{"key":"internal","name":"Internal"},"usage":{"channels":0,"heartbeats":0,"monitors":0}}`+"\n"; got != want {
		t.Fatalf("usage of the default tenant:\n got %s\nwant %s", got, want)
	}

	pgtest.MustExec(t, admin, `UPDATE tenants SET plan_key = 'free' WHERE slug = 'default'`)
	create := func(name string) response {
		return c.do(http.MethodPost, "/api/v1/monitors", `{"name":"`+name+`","url":"https://example.com/`+name+`"}`, unsafe)
	}
	var ids []string
	for _, name := range []string{"a", "b", "c"} {
		r := create(name)
		if r.code != http.StatusCreated {
			t.Fatalf("create %s on free: %d %s", name, r.code, r.raw)
		}
		ids = append(ids, r.body["id"].(string))
	}
	// A paused monitor still counts.
	if r := c.do(http.MethodPost, "/api/v1/monitors/"+ids[0]+"/pause", "", unsafe); r.code != http.StatusOK {
		t.Fatalf("pause: %d %s", r.code, r.raw)
	}
	if r := create("d"); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:plan-limit-reached" ||
		r.body["limit"] != "monitors" || r.body["max"] != float64(3) || r.body["status"] != float64(409) {
		t.Fatalf("4th monitor on free: %d %s", r.code, r.raw)
	}
	if got, want := usage(cookie), `{"limits":{"channels":1,"heartbeats":1,"monitors":3},"plan":{"key":"free","name":"Free"},"usage":{"channels":0,"heartbeats":0,"monitors":3}}`+"\n"; got != want {
		t.Fatalf("usage at the limit:\n got %s\nwant %s", got, want)
	}

	// tenants.limits overrides the plan key by key.
	pgtest.MustExec(t, admin, `UPDATE tenants SET limits = '{"max_monitors": 4}' WHERE slug = 'default'`)
	if r := create("d"); r.code != http.StatusCreated {
		t.Fatalf("create with override: %d %s", r.code, r.raw)
	}
	if r := create("e"); r.code != http.StatusConflict || r.body["max"] != float64(4) {
		t.Fatalf("over the override: %d %s", r.code, r.raw)
	}

	// A deleted monitor frees its slot.
	if r := c.do(http.MethodDelete, "/api/v1/monitors/"+ids[1], "", unsafe); r.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.code, r.raw)
	}
	if got, want := usage(cookie), `{"limits":{"channels":1,"heartbeats":1,"monitors":4},"plan":{"key":"free","name":"Free"},"usage":{"channels":0,"heartbeats":0,"monitors":3}}`+"\n"; got != want {
		t.Fatalf("usage after delete:\n got %s\nwant %s", got, want)
	}
	if r := create("e"); r.code != http.StatusCreated {
		t.Fatalf("create after delete: %d %s", r.code, r.raw)
	}

	// Another tenant sees only its own plan and usage, and its quota is its own.
	other, otherCSRF := otherTenantSession(t, env)
	if got, want := usage(other), `{"limits":{"channels":1,"heartbeats":1,"monitors":3},"plan":{"key":"free","name":"Free"},"usage":{"channels":0,"heartbeats":0,"monitors":0}}`+"\n"; got != want {
		t.Fatalf("usage of the other tenant:\n got %s\nwant %s", got, want)
	}
	if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"o","url":"https://example.com/o"}`, withCSRF(other, otherCSRF)); r.code != http.StatusCreated {
		t.Fatalf("other tenant create: %d %s", r.code, r.raw)
	}
	var live int
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM monitors m JOIN tenants t ON t.id = m.tenant_id
		WHERE t.slug = 'default' AND m.deleted_at IS NULL`).Scan(&live); err != nil || live != 4 {
		t.Fatalf("default tenant live monitors = %d (%v)", live, err)
	}

	if r := c.do(http.MethodGet, "/api/v1/usage", "", nil); r.code != http.StatusUnauthorized {
		t.Fatalf("usage without session: %d", r.code)
	}
}
