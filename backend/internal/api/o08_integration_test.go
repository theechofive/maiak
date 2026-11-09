package api_test

import (
	"net/http"
	"testing"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// loginAs signs a registered user in with the owner password of the tests.
func loginAs(t *testing.T, c *client, email string) response {
	t.Helper()
	r := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"`+email+`","password":"`+ownerPassword+`"}`, map[string]string{"X-Forwarded-For": "198.51.100.22"})
	if r.code != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, r.code, r.raw)
	}
	return r
}

func tenantOf(r response) string {
	tenant, _ := r.body["tenant"].(map[string]any)
	id, _ := tenant["id"].(string)
	return id
}

// O-08: a login enters the organization used last (L-307), member logins are shown to owners and
// admins only (L-278), and the session lists the organization's plan features (ADR-0105).
func TestIntegrationO08Decisions(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("o08"))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	aCookie, aCSRF, aReg := c.register("a@example.com")
	a := withCSRF(aCookie, aCSRF)
	_, _, bReg := c.register("b@example.com")
	tenantA, tenantB := tenantOf(aReg), tenantOf(bReg)

	// Features: none on the seeded plans; the plan's list, sorted, once it has some.
	if me := meOf(t, c, a); me.code != http.StatusOK || me.raw == "" || len(me.body["features"].([]any)) != 0 {
		t.Fatalf("features of a free organization: %d %s", me.code, me.raw)
	}
	pgtest.MustExec(t, env.AdminConn(t), `UPDATE plans SET features = '{sso,audit-export}' WHERE key = 'free'`)
	if me := meOf(t, c, a); len(me.body["features"].([]any)) != 2 || me.body["features"].([]any)[0] != "audit-export" {
		t.Fatalf("features: %s", me.raw)
	}

	// b joins a's organization as a viewer; accepting creates a session there, so it is the last used.
	tok, _ := invite(t, c, a, "viewer")
	bLogin := loginAs(t, c, "b@example.com")
	if got := tenantOf(bLogin); got != tenantB {
		t.Fatalf("first login of b enters %s, want its own %s", got, tenantB)
	}
	bCookie, bCSRF := session(t, bLogin)
	b := withCSRF(bCookie, bCSRF)
	acc := c.do(http.MethodPost, "/api/v1/invitations/accept", `{"token":"`+tok+`"}`, b)
	if acc.code != http.StatusOK || tenantOf(acc) != tenantA {
		t.Fatalf("accept: %d %s", acc.code, acc.raw)
	}
	if got := tenantOf(loginAs(t, c, "b@example.com")); got != tenantA {
		t.Fatalf("login after accepting enters %s, want the last used %s", got, tenantA)
	}
	cookie, csrf := session(t, acc)
	b = withCSRF(cookie, csrf)

	// Member logins: the viewer sees names and roles only, the owner sees logins.
	logins := func(path string, hdr map[string]string) []any {
		t.Helper()
		r := c.do(http.MethodGet, path, "", hdr)
		if r.code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, r.code, r.raw)
		}
		var out []any
		for _, it := range r.body["items"].([]any) {
			out = append(out, it.(map[string]any)["login"])
		}
		return out
	}
	for _, l := range logins("/api/v1/members", b) {
		if l != nil {
			t.Fatalf("viewer sees a login: %v", l)
		}
	}
	for _, l := range logins("/api/v1/members", a) {
		if l == nil {
			t.Fatal("owner does not see a login")
		}
	}
	projects := c.do(http.MethodGet, "/api/v1/projects", "", a)
	def := projects.body["items"].([]any)[0].(map[string]any)["id"].(string)
	for _, l := range logins("/api/v1/projects/"+def+"/members", b) {
		if l != nil {
			t.Fatalf("viewer sees a project member login: %v", l)
		}
	}
	for _, l := range logins("/api/v1/projects/"+def+"/members", a) {
		if l == nil {
			t.Fatal("owner does not see a project member login")
		}
	}

	// Usage: the owner yes, the viewer no (L-288).
	if r := c.do(http.MethodGet, "/api/v1/usage", "", a); r.code != http.StatusOK {
		t.Fatalf("owner usage: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodGet, "/api/v1/usage", "", b); r.code != http.StatusForbidden {
		t.Fatalf("viewer usage: %d %s", r.code, r.raw)
	}

	// Switching back makes b's own organization the last used again.
	sw := c.do(http.MethodPost, "/api/v1/auth/switch-organization", `{"tenantId":"`+tenantB+`"}`, b)
	if sw.code != http.StatusOK {
		t.Fatalf("switch: %d %s", sw.code, sw.raw)
	}
	if got := tenantOf(loginAs(t, c, "b@example.com")); got != tenantB {
		t.Fatalf("login after switching enters %s, want %s", got, tenantB)
	}
}
