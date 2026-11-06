package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// membersOpts gives the member tests generous per-IP limits; TestIntegrationInvitationRateLimit
// checks the invitation limit on its own.
func membersOpts() auth.Options {
	fake := clock.System{}
	return auth.Options{
		IPLimiter:     auth.NewLimiter(1000, time.Minute, 1000, fake),
		InviteLimiter: auth.NewLimiter(1000, time.Minute, 1000, fake),
	}
}

// adminSession logs the platform admin (owner of the default tenant) in.
func adminSession(c *client) map[string]string {
	cookie, csrf := c.login()
	return withCSRF(cookie, csrf)
}

// invite creates an invitation and returns its token and id.
func invite(t *testing.T, c *client, hdr map[string]string, role string) (string, string) {
	t.Helper()
	r := c.do(http.MethodPost, "/api/v1/invitations", `{"role":"`+role+`","label":"for a test"}`, hdr)
	if r.code != http.StatusCreated {
		t.Fatalf("invite %s: %d %s", role, r.code, r.raw)
	}
	link, _ := r.body["inviteUrl"].(string)
	_, token, ok := strings.Cut(link, "#")
	if !ok || !strings.HasPrefix(link, heartbeatPublicURL+"/invite#") || len(token) != 43 {
		t.Fatalf("invite link %q", link)
	}
	inv, _ := r.body["invitation"].(map[string]any)
	return token, inv["id"].(string)
}

// joinByRegister registers a new user through an invitation and returns its session headers.
func joinByRegister(t *testing.T, c *client, token, email string) (map[string]string, response) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"email": email, "password": ownerPassword, "inviteToken": token})
	r := c.do(http.MethodPost, "/api/v1/auth/register", string(b), map[string]string{"X-Forwarded-For": "198.51.100.21"})
	if r.code != http.StatusCreated {
		t.Fatalf("register %s with invitation: %d %s", email, r.code, r.raw)
	}
	cookie, csrf := session(t, r)
	return withCSRF(cookie, csrf), r
}

func meOf(t *testing.T, c *client, hdr map[string]string) response {
	t.Helper()
	return c.do(http.MethodGet, "/api/v1/auth/me", "", hdr)
}

func userIDOf(t *testing.T, c *client, hdr map[string]string) string {
	t.Helper()
	me := meOf(t, c, hdr)
	u, _ := me.body["user"].(map[string]any)
	id, _ := u["id"].(string)
	if id == "" {
		t.Fatalf("me: %d %s", me.code, me.raw)
	}
	return id
}

// roleCalls has the expected minimum role (ADR-0102) and a well-formed request for every operation that needs a session and is not a
// platform admin operation: the request validator runs before the role check, so a malformed
// request would be answered 400 before the role is looked at. Ids are random, so allowed calls
// end in 404 (or 409/422) without touching the members of the test.
var roleCalls = map[string]struct {
	min                authz.Role
	method, path, body string
}{
	"listOrganizations":  {authz.Viewer, http.MethodGet, "/api/v1/auth/organizations", ""},
	"switchOrganization": {authz.Viewer, http.MethodPost, "/api/v1/auth/switch-organization", `{"tenantId":"0192b9c4-7c5e-7000-8000-0000000000ff"}`},
	"getMe":              {authz.Viewer, http.MethodGet, "/api/v1/auth/me", ""},
	// Status page (ADR-0103): settings readable by everyone, changes for admins and owners.
	"listStatusPages":          {authz.Viewer, http.MethodGet, "/api/v1/status-pages", ""},
	"createStatusPage":         {authz.Admin, http.MethodPost, "/api/v1/status-pages", `{"slug":"role-matrix","title":"Role matrix"}`},
	"getStatusPage":            {authz.Viewer, http.MethodGet, "/api/v1/status-pages/{id}", ""},
	"updateStatusPage":         {authz.Admin, http.MethodPatch, "/api/v1/status-pages/{id}", `{"title":"x"}`},
	"deleteStatusPage":         {authz.Admin, http.MethodDelete, "/api/v1/status-pages/{id}", ""},
	"setStatusPagePassword":    {authz.Admin, http.MethodPut, "/api/v1/status-pages/{id}/password", `{"password":"twelve chars long"}`},
	"clearStatusPagePassword":  {authz.Admin, http.MethodDelete, "/api/v1/status-pages/{id}/password", ""},
	"listStatusPageComponents": {authz.Admin, http.MethodGet, "/api/v1/status-pages/{id}/components", ""},
	"putStatusPageComponents":  {authz.Admin, http.MethodPut, "/api/v1/status-pages/{id}/components", `{"items":[]}`},
	"previewStatusPage":        {authz.Admin, http.MethodGet, "/api/v1/status-pages/{id}/preview", ""},
	"listMonitors":             {authz.Viewer, http.MethodGet, "/api/v1/monitors", ""},
	"createMonitor":            {authz.Member, http.MethodPost, "/api/v1/monitors", `{"name":"role matrix","url":"https://example.com/role"}`},
	"getMonitor":               {authz.Viewer, http.MethodGet, "/api/v1/monitors/{id}", ""},
	"updateMonitor":            {authz.Member, http.MethodPatch, "/api/v1/monitors/{id}", `{"name":"x"}`},
	"deleteMonitor":            {authz.Member, http.MethodDelete, "/api/v1/monitors/{id}", ""},
	"pauseMonitor":             {authz.Member, http.MethodPost, "/api/v1/monitors/{id}/pause", ""},
	"resumeMonitor":            {authz.Member, http.MethodPost, "/api/v1/monitors/{id}/resume", ""},
	"regenerateHeartbeatToken": {authz.Member, http.MethodPost, "/api/v1/monitors/{id}/heartbeat-token", ""},
	"getMonitorStats":          {authz.Viewer, http.MethodGet, "/api/v1/monitors/{id}/stats", ""},
	"listMonitorChecks":        {authz.Viewer, http.MethodGet, "/api/v1/monitors/{id}/checks", ""},
	"listMonitorIncidents":     {authz.Viewer, http.MethodGet, "/api/v1/monitors/{id}/incidents", ""},
	"getUsage":                 {authz.Viewer, http.MethodGet, "/api/v1/usage", ""},
	"listChannels":             {authz.Viewer, http.MethodGet, "/api/v1/channels", ""},
	"createChannel":            {authz.Admin, http.MethodPost, "/api/v1/channels", `{"name":"role","webhookUrl":"https://hooks.slack.com/services/T000/B000/xyz"}`},
	"getChannel":               {authz.Viewer, http.MethodGet, "/api/v1/channels/{id}", ""},
	"updateChannel":            {authz.Admin, http.MethodPatch, "/api/v1/channels/{id}", `{"name":"x"}`},
	"deleteChannel":            {authz.Admin, http.MethodDelete, "/api/v1/channels/{id}", ""},
	"testChannel":              {authz.Admin, http.MethodPost, "/api/v1/channels/{id}/test", ""},
	"listMembers":              {authz.Viewer, http.MethodGet, "/api/v1/members", ""},
	"updateMember":             {authz.Admin, http.MethodPatch, "/api/v1/members/{id}", `{"role":"viewer"}`},
	"removeMember":             {authz.Admin, http.MethodDelete, "/api/v1/members/{id}", ""},
	"listInvitations":          {authz.Admin, http.MethodGet, "/api/v1/invitations", ""},
	"createInvitation":         {authz.Admin, http.MethodPost, "/api/v1/invitations", `{"role":"viewer"}`},
	"revokeInvitation":         {authz.Admin, http.MethodPost, "/api/v1/invitations/{id}/revoke", ""},
	"acceptInvitation":         {authz.Viewer, http.MethodPost, "/api/v1/invitations/accept", `{"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`},
	"deleteOrganization":       {authz.Owner, http.MethodDelete, "/api/v1/organization", ""},
	"listProjects":             {authz.Viewer, http.MethodGet, "/api/v1/projects", ""},
	"createProject":            {authz.Admin, http.MethodPost, "/api/v1/projects", `{"name":"role matrix"}`},
	"getProject":               {authz.Viewer, http.MethodGet, "/api/v1/projects/{id}", ""},
	"updateProject":            {authz.Admin, http.MethodPatch, "/api/v1/projects/{id}", `{"name":"x"}`},
	"deleteProject":            {authz.Admin, http.MethodDelete, "/api/v1/projects/{id}", ""},
	"listProjectMembers":       {authz.Viewer, http.MethodGet, "/api/v1/projects/{id}/members", ""},
	"putProjectMember":         {authz.Admin, http.MethodPut, "/api/v1/projects/{id}/members/{id}", `{"role":"viewer"}`},
	"removeProjectMember":      {authz.Admin, http.MethodDelete, "/api/v1/projects/{id}/members/{id}", ""},
	// Allowed calls of these two end the session; they are checked in the lifecycle tests.
	"logout":            {authz.Viewer, http.MethodPost, "/api/v1/auth/logout", ""},
	"leaveOrganization": {authz.Viewer, http.MethodPost, "/api/v1/organization/leave", ""},
}

// TestIntegrationRoleMatrix calls every operation of the contract that needs a session as an
// owner, admin, member and viewer of the same organization: a role below the operation's minimum
// gets 403 forbidden, any other role is not refused by role.
func TestIntegrationRoleMatrix(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("rolematrix"))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	owner := adminSession(c)
	sessions := map[authz.Role]map[string]string{authz.Owner: owner}
	for _, role := range []authz.Role{authz.Admin, authz.Member, authz.Viewer} {
		token, _ := invite(t, c, owner, string(role))
		sessions[role], _ = joinByRegister(t, c, token, string(role)+"@example.com")
	}

	_, ops := specOpsForTest(t)
	checked := 0
	for _, op := range ops {
		min, ok := authz.MinRole(op.id)
		if !ok {
			t.Errorf("%s not in the authorization table", op.id)
			continue
		}
		if min == authz.Public || strings.HasPrefix(op.path, "/api/v1/admin/") {
			continue
		}
		call, ok := roleCalls[lowerFirst(op.id)]
		if !ok {
			t.Errorf("%s: no request in roleCalls; add one", op.id)
			continue
		}
		// The expected minimum role is written down here independently of the table, so a change of
		// the table without a change of this test fails.
		if call.min != min {
			t.Errorf("%s: minimum role in the table %s, expected %s", op.id, min, call.min)
		}
		path := strings.ReplaceAll(call.path, "{id}", "0192b9c4-7c5e-7000-8000-0000000000aa")
		for _, role := range []authz.Role{authz.Viewer, authz.Member, authz.Admin, authz.Owner} {
			allowed := role.AtLeast(min)
			if allowed && (op.id == "Logout" || op.id == "LeaveOrganization" || strings.EqualFold(op.id, "logout") || strings.EqualFold(op.id, "leaveOrganization")) {
				continue
			}
			r := c.do(call.method, path, call.body, sessions[role])
			forbidden := r.code == http.StatusForbidden && r.body["type"] == "urn:maiak:problem:forbidden"
			if forbidden == allowed {
				t.Errorf("%s as %s: %d %s (min role %s)", op.id, role, r.code, r.raw, min)
			}
			if r.code == http.StatusBadRequest || r.code == http.StatusUnauthorized || r.code >= 500 {
				t.Errorf("%s as %s: unexpected %d %s", op.id, role, r.code, r.raw)
			}
		}
		checked++
	}
	if checked < 30 {
		t.Fatalf("only %d operations checked", checked)
	}
	// Every session survived the matrix.
	for role, hdr := range sessions {
		if r := meOf(t, c, hdr); r.code != http.StatusOK || r.body["role"] != string(role) {
			t.Fatalf("%s after the matrix: %d %s", role, r.code, r.raw)
		}
	}
}

func lowerFirst(s string) string { return strings.ToLower(s[:1]) + s[1:] }

type opForTest struct{ id, method, path string }

func specOpsForTest(t *testing.T) (int, []opForTest) {
	t.Helper()
	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	var out []opForTest
	for _, path := range spec.Paths.InMatchingOrder() {
		for method, op := range spec.Paths.Value(path).Operations() {
			out = append(out, opForTest{id: op.OperationID, method: method, path: path})
		}
	}
	return len(out), out
}

// TestIntegrationInvitationLifecycle: the link is shown once and only its hash stored; a new
// person registers through it with registration turned off and joins without a tenant of its
// own; it cannot be used twice; revoked and expired links are refused with the same answer;
// a logged-in user of another organization accepts one and the session switches.
func TestIntegrationInvitationLifecycle(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("invitelife"))
	logs := &lockedBuffer{}
	c := newRegistrationServer(t, env, membersOpts(), false, logs)
	owner := adminSession(c)
	ctx := context.Background()
	admin := env.AdminConn(t)

	token, invID := invite(t, c, owner, "member")
	if n := countRows(t, env, `SELECT count(*) FROM invitations WHERE id = $1 AND token_hash = sha256(convert_to($2, 'UTF8'))`, invID, token); n != 1 {
		t.Fatal("the stored hash is not the SHA-256 of the token")
	}
	if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE metadata::text LIKE '%' || $1 || '%'`, token); n != 0 {
		t.Fatal("token in the audit log")
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("token in the logs")
	}
	list := c.do(http.MethodGet, "/api/v1/invitations", "", owner)
	if list.code != http.StatusOK || strings.Contains(list.raw, token) || !strings.Contains(list.raw, `"status":"open"`) {
		t.Fatalf("list: %d %s", list.code, list.raw)
	}

	// The public lookup.
	lookup := func(tok string) response {
		return c.do(http.MethodPost, "/api/v1/invitations/lookup", `{"token":"`+tok+`"}`, nil)
	}
	if r := lookup(token); r.code != http.StatusOK || r.body["organizationName"] != "Default" || r.body["role"] != "member" {
		t.Fatalf("lookup: %d %s", r.code, r.raw)
	}

	// Register through it although registration is turned off; no tenant of its own.
	tenantsBefore := countRows(t, env, `SELECT count(*) FROM tenants`)
	if r := c.do(http.MethodPost, "/api/v1/auth/register", registerBody("plain@example.com", ownerPassword), nil); r.code != http.StatusForbidden {
		t.Fatalf("plain registration with registration off: %d", r.code)
	}
	joined, r := joinByRegister(t, c, token, "new@example.com")
	if tenant, _ := r.body["tenant"].(map[string]any); tenant["slug"] != "default" || r.body["role"] != "member" {
		t.Fatalf("joined: %s", r.raw)
	}
	if n := countRows(t, env, `SELECT count(*) FROM tenants`); n != tenantsBefore {
		t.Fatalf("registration through an invitation created a tenant (%d -> %d)", tenantsBefore, n)
	}
	if r := meOf(t, c, joined); r.code != http.StatusOK || r.body["role"] != "member" {
		t.Fatalf("me of the new member: %d %s", r.code, r.raw)
	}
	if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE action = 'member.joined'`); n != 1 {
		t.Fatalf("member.joined rows: %d", n)
	}

	// Used once: lookup, a second registration and an accept all get the same 404.
	b, _ := json.Marshal(map[string]string{"email": "second@example.com", "password": ownerPassword, "inviteToken": token})
	for name, r := range map[string]response{
		"lookup":   lookup(token),
		"register": c.do(http.MethodPost, "/api/v1/auth/register", string(b), nil),
		"accept":   c.do(http.MethodPost, "/api/v1/invitations/accept", `{"token":"`+token+`"}`, owner),
		"garbage":  lookup("not-a-token"),
	} {
		if r.code != http.StatusNotFound || r.body["type"] != "urn:maiak:problem:invitation-invalid" {
			t.Errorf("used token %s: %d %s", name, r.code, r.raw)
		}
	}
	if list := c.do(http.MethodGet, "/api/v1/invitations", "", owner); !strings.Contains(list.raw, `"status":"accepted"`) || !strings.Contains(list.raw, `"acceptedBy":"new@example.com"`) {
		t.Fatalf("list after accept: %s", list.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/invitations/"+invID+"/revoke", "", owner); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:invitation-accepted" {
		t.Fatalf("revoke accepted: %d %s", r.code, r.raw)
	}

	// Revoked and expired.
	revoked, revokedID := invite(t, c, owner, "viewer")
	if r := c.do(http.MethodPost, "/api/v1/invitations/"+revokedID+"/revoke", "", owner); r.code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/invitations/"+revokedID+"/revoke", "", owner); r.code != http.StatusNoContent {
		t.Fatalf("revoke twice: %d %s", r.code, r.raw)
	}
	expired, expiredID := invite(t, c, owner, "viewer")
	pgtest.MustExec(t, admin, `UPDATE invitations SET created_at = now() - interval '8 days', expires_at = now() - interval '1 day' WHERE id = '`+expiredID+`'`)
	for name, tok := range map[string]string{"revoked": revoked, "expired": expired} {
		if r := lookup(tok); r.code != http.StatusNotFound || r.body["type"] != "urn:maiak:problem:invitation-invalid" {
			t.Errorf("%s lookup: %d %s", name, r.code, r.raw)
		}
	}
	if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE action = 'invitation.revoked'`); n != 1 {
		t.Fatalf("invitation.revoked rows: %d", n)
	}

	// A logged-in user of another organization accepts; the session moves there.
	otherCookie, otherCSRF := otherTenantSession(t, env)
	other := withCSRF(otherCookie, otherCSRF)
	tok, _ := invite(t, c, owner, "viewer")
	acc := c.do(http.MethodPost, "/api/v1/invitations/accept", `{"token":"`+tok+`"}`, other)
	if acc.code != http.StatusOK || acc.body["role"] != "viewer" {
		t.Fatalf("accept: %d %s", acc.code, acc.raw)
	}
	if r := meOf(t, c, other); r.code != http.StatusUnauthorized {
		t.Fatalf("old session after accept: %d", r.code)
	}
	newCookie, newCSRF := session(t, acc)
	moved := withCSRF(newCookie, newCSRF)
	if r := meOf(t, c, moved); r.code != http.StatusOK || r.body["tenant"].(map[string]any)["slug"] != "default" {
		t.Fatalf("new session: %d %s", r.code, r.raw)
	}
	// Already a member.
	again, _ := invite(t, c, owner, "viewer")
	if r := c.do(http.MethodPost, "/api/v1/invitations/accept", `{"token":"`+again+`"}`, moved); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:already-member" {
		t.Fatalf("accept as member: %d %s", r.code, r.raw)
	}
	// The invitation stays open after the refusal.
	if r := lookup(again); r.code != http.StatusOK {
		t.Fatalf("invitation used up by a refused accept: %d", r.code)
	}
	var orgs int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id WHERE u.email = 'other@example.com'`).Scan(&orgs); err != nil || orgs != 2 {
		t.Fatalf("memberships of the accepting user: %d %v", orgs, err)
	}
}

// The platform admin cannot join another organization: BootstrapAdmin refuses to start when it
// belongs to one (ADR-0095).
func TestIntegrationPlatformAdminCannotJoin(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("invitepadmin"))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	ownerCookie, ownerCSRF, _ := c.register("owner@example.com")
	tok, _ := invite(t, c, withCSRF(ownerCookie, ownerCSRF), "admin")
	r := c.do(http.MethodPost, "/api/v1/invitations/accept", `{"token":"`+tok+`"}`, adminSession(c))
	if r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:already-member" {
		t.Fatalf("platform admin accept: %d %s", r.code, r.raw)
	}
}

// TestIntegrationMemberManagement: role changes, removals and their guard rails.
func TestIntegrationMemberManagement(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("members"))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	oCookie, oCSRF, _ := c.register("owner@example.com")
	owner := withCSRF(oCookie, oCSRF)
	ownerID := userIDOf(t, c, owner)

	// The only owner can neither step down nor leave.
	if r := c.do(http.MethodPatch, "/api/v1/members/"+ownerID, `{"role":"admin"}`, owner); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:last-owner" {
		t.Fatalf("demote last owner: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/organization/leave", "", owner); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:last-owner" {
		t.Fatalf("last owner leaves: %d %s", r.code, r.raw)
	}

	tok, _ := invite(t, c, owner, "admin")
	adm, _ := joinByRegister(t, c, tok, "admin@example.com")
	admID := userIDOf(t, c, adm)
	tok, _ = invite(t, c, owner, "member")
	mem, _ := joinByRegister(t, c, tok, "member@example.com")
	memID := userIDOf(t, c, mem)

	// Admins: no owner grants, no touching owners, but other roles yes.
	for name, call := range map[string][3]string{
		"grant owner":  {http.MethodPatch, "/api/v1/members/" + memID, `{"role":"owner"}`},
		"demote owner": {http.MethodPatch, "/api/v1/members/" + ownerID, `{"role":"viewer"}`},
		"remove owner": {http.MethodDelete, "/api/v1/members/" + ownerID, ""},
	} {
		if r := c.do(call[0], call[1], call[2], adm); r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:forbidden" {
			t.Errorf("admin %s: %d %s", name, r.code, r.raw)
		}
	}
	if r := c.do(http.MethodPost, "/api/v1/invitations", `{"role":"owner"}`, adm); r.code != http.StatusBadRequest {
		t.Errorf("owner invitation: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/members/"+memID, `{"role":"viewer"}`, adm); r.code != http.StatusOK || r.body["role"] != "viewer" {
		t.Fatalf("admin demotes member: %d %s", r.code, r.raw)
	}
	// The new role applies on the next request.
	if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"x","url":"https://example.com"}`, mem); r.code != http.StatusForbidden {
		t.Fatalf("viewer creates a monitor: %d %s", r.code, r.raw)
	}
	if r := meOf(t, c, mem); r.body["role"] != "viewer" {
		t.Fatalf("me after demotion: %s", r.raw)
	}

	// Ownership transfer: grant owner, then step down.
	if r := c.do(http.MethodPatch, "/api/v1/members/"+admID, `{"role":"owner"}`, owner); r.code != http.StatusOK {
		t.Fatalf("grant owner: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/members/"+ownerID, `{"role":"admin"}`, owner); r.code != http.StatusOK {
		t.Fatalf("step down: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/members/"+admID, `{"role":"admin"}`, adm); r.code != http.StatusConflict {
		t.Fatalf("new last owner steps down: %d %s", r.code, r.raw)
	}

	// Removing a member ends its sessions at once.
	if r := c.do(http.MethodDelete, "/api/v1/members/"+memID, "", owner); r.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", r.code, r.raw)
	}
	if r := meOf(t, c, mem); r.code != http.StatusUnauthorized {
		t.Fatalf("removed member's session: %d", r.code)
	}
	if r := c.do(http.MethodDelete, "/api/v1/members/"+memID, "", owner); r.code != http.StatusNotFound {
		t.Fatalf("remove twice: %d", r.code)
	}

	// The old owner (now admin) leaves.
	if r := c.do(http.MethodPost, "/api/v1/organization/leave", "", owner); r.code != http.StatusNoContent {
		t.Fatalf("leave: %d %s", r.code, r.raw)
	}
	if r := meOf(t, c, owner); r.code != http.StatusUnauthorized {
		t.Fatalf("session after leaving: %d", r.code)
	}
	list := c.do(http.MethodGet, "/api/v1/members", "", adm)
	if items, _ := list.body["items"].([]any); len(items) != 1 {
		t.Fatalf("members: %s", list.raw)
	}
	want := "member.joined,member.joined,member.role_changed,member.role_changed,member.role_changed,member.removed,member.removed"
	var got []string
	rows, err := env.AdminConn(t).Query(context.Background(), `SELECT action FROM audit_log WHERE action LIKE 'member.%' AND action <> 'member.invited' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		got = append(got, a)
	}
	if strings.Join(got, ",") != want {
		t.Fatalf("audit = %v\nwant %s", got, want)
	}

	// The platform admin of the default tenant cannot be demoted or removed.
	padmin := adminSession(c)
	padminID := userIDOf(t, c, padmin)
	tok, _ = invite(t, c, padmin, "admin")
	_, _ = joinByRegister(t, c, tok, "second-owner@example.com")
	for _, call := range [][3]string{
		{http.MethodPatch, "/api/v1/members/" + padminID, `{"role":"admin"}`},
		{http.MethodDelete, "/api/v1/members/" + padminID, ""},
		{http.MethodPost, "/api/v1/organization/leave", ""},
	} {
		if r := c.do(call[0], call[1], call[2], padmin); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:system-admin" {
			t.Errorf("%s %s on the platform admin: %d %s", call[0], call[1], r.code, r.raw)
		}
	}
}

// Two owners demoting each other at the same time: the tenant row lock serialises them, so the
// organization keeps one owner.
func TestIntegrationConcurrentOwnerDemotion(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("ownerrace"))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	oCookie, oCSRF, _ := c.register("a@example.com")
	a := withCSRF(oCookie, oCSRF)
	tok, _ := invite(t, c, a, "admin")
	b, _ := joinByRegister(t, c, tok, "b@example.com")
	aID, bID := userIDOf(t, c, a), userIDOf(t, c, b)
	if r := c.do(http.MethodPatch, "/api/v1/members/"+bID, `{"role":"owner"}`, a); r.code != http.StatusOK {
		t.Fatalf("grant: %d %s", r.code, r.raw)
	}
	for range 5 {
		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[0] = c.do(http.MethodPatch, "/api/v1/members/"+bID, `{"role":"admin"}`, a).code
		}()
		go func() {
			defer wg.Done()
			codes[1] = c.do(http.MethodPatch, "/api/v1/members/"+aID, `{"role":"admin"}`, b).code
		}()
		wg.Wait()
		if n := countRows(t, env, `SELECT count(*) FROM memberships WHERE role = 'owner' AND user_id IN ($1, $2)`, aID, bID); n != 1 {
			t.Fatalf("owners after concurrent demotion: %d (codes %v)", n, codes)
		}
		// Restore two owners: whoever is still owner promotes the other.
		pgtest.MustExec(t, env.AdminConn(t), `UPDATE memberships SET role = 'owner' WHERE user_id IN ('`+aID+`', '`+bID+`')`)
	}
}

// TestIntegrationMemberQuota: members plus open invitations are limited by max_members (free: 3);
// concurrent invitations never pass the limit, and concurrent registrations through one token
// make one member.
func TestIntegrationMemberQuota(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("memberquota"))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	oCookie, oCSRF, _ := c.register("owner@example.com")
	owner := withCSRF(oCookie, oCSRF)

	t1, _ := invite(t, c, owner, "member")
	_, id2 := invite(t, c, owner, "viewer")
	r := c.do(http.MethodPost, "/api/v1/invitations", `{"role":"viewer"}`, owner)
	if r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:plan-limit-reached" || r.body["limit"] != "members" || r.body["max"] != float64(3) {
		t.Fatalf("over the limit: %d %s", r.code, r.raw)
	}
	if u := c.do(http.MethodGet, "/api/v1/usage", "", owner); !strings.Contains(u.raw, `"members":3`) {
		t.Fatalf("usage: %s", u.raw)
	}
	// Revoking frees a place; concurrent invitations take it once.
	if r := c.do(http.MethodPost, "/api/v1/invitations/"+id2+"/revoke", "", owner); r.code != http.StatusNoContent {
		t.Fatalf("revoke: %d", r.code)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- c.do(http.MethodPost, "/api/v1/invitations", `{"role":"viewer"}`, owner).code
		}()
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		if code == http.StatusCreated {
			created++
		} else if code != http.StatusConflict {
			t.Errorf("concurrent invite: %d", code)
		}
	}
	if created != 1 {
		t.Fatalf("%d invitations created for one free place", created)
	}

	// One token, several registrations at once: exactly one member.
	var wg2 sync.WaitGroup
	results := make(chan int, 4)
	for i := range 4 {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			b, _ := json.Marshal(map[string]string{"email": fmt.Sprintf("racer%d@example.com", i), "password": ownerPassword, "inviteToken": t1})
			results <- c.do(http.MethodPost, "/api/v1/auth/register", string(b), nil).code
		}()
	}
	wg2.Wait()
	close(results)
	ok := 0
	for code := range results {
		if code == http.StatusCreated {
			ok++
		} else if code != http.StatusNotFound {
			t.Errorf("racing registration: %d", code)
		}
	}
	if ok != 1 {
		t.Fatalf("%d registrations through one token", ok)
	}
	if n := countRows(t, env, `SELECT count(*) FROM users WHERE email LIKE 'racer%'`); n != 1 {
		t.Fatalf("%d racer users kept (the losers must roll back)", n)
	}
	if u := c.do(http.MethodGet, "/api/v1/usage", "", owner); !strings.Contains(u.raw, `"members":3`) {
		t.Fatalf("usage after joins: %s", u.raw)
	}
}

// TestIntegrationInvitationIsolation: invitations and members of one organization are invisible
// and untouchable from another.
func TestIntegrationInvitationIsolation(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("inviteiso"))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	aCookie, aCSRF, _ := c.register("a@example.com")
	a := withCSRF(aCookie, aCSRF)
	bCookie, bCSRF, _ := c.register("b@example.com")
	b := withCSRF(bCookie, bCSRF)
	_, invID := invite(t, c, a, "viewer")
	aID := userIDOf(t, c, a)

	if r := c.do(http.MethodGet, "/api/v1/invitations", "", b); r.code != http.StatusOK || strings.Contains(r.raw, invID) {
		t.Fatalf("b lists a's invitation: %s", r.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/invitations/"+invID+"/revoke", "", b); r.code != http.StatusNotFound {
		t.Fatalf("b revokes a's invitation: %d", r.code)
	}
	if r := c.do(http.MethodGet, "/api/v1/members", "", b); strings.Contains(r.raw, aID) {
		t.Fatalf("b lists a: %s", r.raw)
	}
	for _, call := range [][3]string{
		{http.MethodPatch, "/api/v1/members/" + aID, `{"role":"viewer"}`},
		{http.MethodDelete, "/api/v1/members/" + aID, ""},
	} {
		if r := c.do(call[0], call[1], call[2], b); r.code != http.StatusNotFound {
			t.Errorf("b %s a: %d %s", call[0], r.code, r.raw)
		}
	}
	if n := countRows(t, env, `SELECT count(*) FROM invitations WHERE revoked_at IS NULL`); n != 1 {
		t.Fatalf("open invitations: %d", n)
	}
}

// TestIntegrationSwitchOrganization: the switcher lists the user's organizations, switching
// replaces the session, a suspended organization and a foreign one are refused.
func TestIntegrationSwitchOrganization(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("switchorg"))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	aCookie, aCSRF, aReg := c.register("a@example.com")
	a := withCSRF(aCookie, aCSRF)
	bCookie, bCSRF, bReg := c.register("b@example.com")
	b := withCSRF(bCookie, bCSRF)
	tenantA := aReg.body["tenant"].(map[string]any)["id"].(string)
	tenantB := bReg.body["tenant"].(map[string]any)["id"].(string)
	tok, _ := invite(t, c, a, "viewer")
	acc := c.do(http.MethodPost, "/api/v1/invitations/accept", `{"token":"`+tok+`"}`, b)
	if acc.code != http.StatusOK {
		t.Fatalf("accept: %d %s", acc.code, acc.raw)
	}
	cookie, csrf := session(t, acc)
	b = withCSRF(cookie, csrf)

	orgs := c.do(http.MethodGet, "/api/v1/auth/organizations", "", b)
	items, _ := orgs.body["items"].([]any)
	if orgs.code != http.StatusOK || len(items) != 2 {
		t.Fatalf("organizations: %d %s", orgs.code, orgs.raw)
	}
	for _, it := range items {
		o := it.(map[string]any)
		if (o["id"] == tenantA) != (o["current"] == true) {
			t.Fatalf("current flag: %s", orgs.raw)
		}
	}

	sw := c.do(http.MethodPost, "/api/v1/auth/switch-organization", `{"tenantId":"`+tenantB+`"}`, b)
	if sw.code != http.StatusOK || sw.body["role"] != "owner" || sw.body["tenant"].(map[string]any)["id"] != tenantB {
		t.Fatalf("switch: %d %s", sw.code, sw.raw)
	}
	if r := meOf(t, c, b); r.code != http.StatusUnauthorized {
		t.Fatalf("old session after switch: %d", r.code)
	}
	cookie, csrf = session(t, sw)
	b = withCSRF(cookie, csrf)
	if r := meOf(t, c, b); r.code != http.StatusOK || r.body["tenant"].(map[string]any)["id"] != tenantB {
		t.Fatalf("me after switch: %s", r.raw)
	}
	if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE action = 'organization.switched' AND tenant_id = $1`, tenantB); n != 1 {
		t.Fatalf("organization.switched rows: %d", n)
	}

	// A foreign organization and an unknown id are 404; a suspended one is refused.
	_, _, cReg := c.register("c@example.com")
	tenantC := cReg.body["tenant"].(map[string]any)["id"].(string)
	for _, id := range []string{tenantC, "0192b9c4-7c5e-7000-8000-0000000000ff"} {
		if r := c.do(http.MethodPost, "/api/v1/auth/switch-organization", `{"tenantId":"`+id+`"}`, b); r.code != http.StatusNotFound {
			t.Fatalf("switch to %s: %d %s", id, r.code, r.raw)
		}
	}
	pgtest.MustExec(t, env.AdminConn(t), `UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantA+`'`)
	if r := c.do(http.MethodPost, "/api/v1/auth/switch-organization", `{"tenantId":"`+tenantA+`"}`, b); r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:account-suspended" {
		t.Fatalf("switch to suspended: %d %s", r.code, r.raw)
	}
	if r := meOf(t, c, b); r.code != http.StatusOK {
		t.Fatalf("session after a refused switch: %d", r.code)
	}
	orgs = c.do(http.MethodGet, "/api/v1/auth/organizations", "", b)
	if !strings.Contains(orgs.raw, `"status":"suspended"`) {
		t.Fatalf("suspended organization not listed as such: %s", orgs.raw)
	}

	// The owner deletes its organization: sessions there end, other memberships stay.
	if r := c.do(http.MethodDelete, "/api/v1/organization", "", b); r.code != http.StatusNoContent {
		t.Fatalf("delete organization: %d %s", r.code, r.raw)
	}
	if r := meOf(t, c, b); r.code != http.StatusUnauthorized {
		t.Fatalf("session in a deleted organization: %d", r.code)
	}
	if r := c.do(http.MethodDelete, "/api/v1/organization", "", adminSession(c)); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:protected-tenant" {
		t.Fatalf("delete default organization: %d %s", r.code, r.raw)
	}
}

// Failed invitation attempts are limited per client IP; a success does not count.
func TestIntegrationInvitationRateLimit(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("inviterate"))
	fake := clock.NewFake(time.Now())
	c := newRegistrationServer(t, env, auth.Options{Clock: fake, InviteLimiter: auth.NewLimiter(3, time.Minute, 100, fake)}, true, &lockedBuffer{})
	ip := map[string]string{"X-Forwarded-For": "203.0.113.50"}
	bad := `{"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`
	for i := range 3 {
		if r := c.do(http.MethodPost, "/api/v1/invitations/lookup", bad, ip); r.code != http.StatusNotFound {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	r := c.do(http.MethodPost, "/api/v1/invitations/lookup", bad, ip)
	if r.code != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Fatalf("limited: %d %s", r.code, r.raw)
	}
	// Another client is not affected; after the window the first one may try again.
	if r := c.do(http.MethodPost, "/api/v1/invitations/lookup", bad, map[string]string{"X-Forwarded-For": "203.0.113.51"}); r.code != http.StatusNotFound {
		t.Fatalf("other client: %d", r.code)
	}
	fake.Advance(time.Minute)
	if r := c.do(http.MethodPost, "/api/v1/invitations/lookup", bad, ip); r.code != http.StatusNotFound {
		t.Fatalf("after the window: %d", r.code)
	}
}

func registerWithInvite(c *client, email, password, token, ip string) response {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password, "inviteToken": token})
	return c.do(http.MethodPost, "/api/v1/auth/register", string(b), map[string]string{"X-Forwarded-For": ip})
}

// T-12 (M1): a user removed from its only organization cannot log in (the generic failure), and
// gets back in through a new invitation by registering with the same email and its password. A
// wrong password is the login's generic answer and leaves the invitation open.
func TestIntegrationInviteExistingAccount(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("inviteexisting"))
	c := newRegistrationServer(t, env, membersOpts(), false, &lockedBuffer{})
	owner := adminSession(c)

	tok, _ := invite(t, c, owner, "member")
	carol, _ := joinByRegister(t, c, tok, "carol@example.com")
	carolID := userIDOf(t, c, carol)
	if r := c.do(http.MethodDelete, "/api/v1/members/"+carolID, "", owner); r.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", r.code, r.raw)
	}
	loginBody := `{"login":"carol@example.com","password":"` + ownerPassword + `"}`
	if r := c.do(http.MethodPost, "/api/v1/auth/login", loginBody, nil); r.code != http.StatusUnauthorized || r.body["type"] != "urn:maiak:problem:invalid-credentials" {
		t.Fatalf("login without memberships: %d %s", r.code, r.raw)
	}

	tok, _ = invite(t, c, owner, "viewer")
	r := registerWithInvite(c, "carol@example.com", "a wrong passphrase", tok, "198.51.100.30")
	if r.code != http.StatusUnauthorized || r.body["type"] != "urn:maiak:problem:invalid-credentials" {
		t.Fatalf("wrong password: %d %s", r.code, r.raw)
	}
	if l := c.do(http.MethodPost, "/api/v1/invitations/lookup", `{"token":"`+tok+`"}`, nil); l.code != http.StatusOK {
		t.Fatalf("invitation consumed by a wrong password: %d", l.code)
	}
	if n := countRows(t, env, `SELECT count(*) FROM users WHERE email = 'carol@example.com'`); n != 1 {
		t.Fatalf("users: %d", n)
	}

	r = registerWithInvite(c, "Carol@Example.com", ownerPassword, tok, "198.51.100.30")
	if r.code != http.StatusCreated || r.body["role"] != "viewer" || r.body["tenant"].(map[string]any)["slug"] != "default" {
		t.Fatalf("existing account joins: %d %s", r.code, r.raw)
	}
	if u := r.body["user"].(map[string]any); u["id"] != carolID {
		t.Fatalf("joined as another user: %v", u)
	}
	if r := c.do(http.MethodPost, "/api/v1/auth/login", loginBody, nil); r.code != http.StatusOK {
		t.Fatalf("login after rejoining: %d %s", r.code, r.raw)
	}
	if n := countRows(t, env, `SELECT count(*) FROM audit_log WHERE action = 'auth.login.failed' AND metadata->>'reason' = 'invitation_invalid_credentials'`); n != 1 {
		t.Fatalf("failed attempt audit rows: %d", n)
	}
	// Used now.
	if r := registerWithInvite(c, "carol@example.com", ownerPassword, tok, "198.51.100.30"); r.code != http.StatusNotFound {
		t.Fatalf("reuse: %d %s", r.code, r.raw)
	}
}

// T-12 (L2): wrong passwords for an existing account are limited per invitation, whatever the
// client address.
func TestIntegrationInviteExistingAccountLimit(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("inviteexistlimit"))
	fake := clock.NewFake(time.Now())
	c := newRegistrationServer(t, env, auth.Options{Clock: fake, IPLimiter: auth.NewLimiter(1000, time.Minute, 1000, fake),
		InviteLimiter: auth.NewLimiter(3, time.Minute, 1000, fake)}, true, &lockedBuffer{})
	c.register("dave@example.com")
	owner := adminSession(c)
	tok, _ := invite(t, c, owner, "viewer")
	for i := range 3 {
		if r := registerWithInvite(c, "dave@example.com", "a wrong passphrase", tok, fmt.Sprintf("203.0.113.%d", 60+i)); r.code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d %s", i, r.code, r.raw)
		}
	}
	if r := registerWithInvite(c, "dave@example.com", ownerPassword, tok, "203.0.113.99"); r.code != http.StatusTooManyRequests {
		t.Fatalf("fourth attempt on the invitation from a new address: %d %s", r.code, r.raw)
	}
}
