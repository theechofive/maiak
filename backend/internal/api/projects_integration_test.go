package api_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// projectWorld is an organization with four projects and people in them (ADR-0102, T-02):
//   - Default (every member), alpha (member, viewer), beta (member with the viewer override),
//     gamma (nobody but owners and admins);
//   - one monitor per project, a heartbeat in gamma, one channel per project and an
//     organization-wide channel.
type projectWorld struct {
	c                         *client
	env                       *pgtest.Env
	sessions                  map[authz.Role]map[string]string
	userIDs                   map[authz.Role]string
	def, alpha, beta, gamma   string
	mDef, mAlpha, mBeta, mGam string
	hbGamma                   string
	chDef, chAlpha, chBeta    string
	chGamma, chOrg            string
	monitorName, channelName  map[string]string
	projectNames              map[string]string
}

func newProjectWorld(t *testing.T, name string) *projectWorld {
	t.Helper()
	env := pgtest.New(t, pgtest.DBName(name))
	c := newRegistrationServer(t, env, membersOpts(), true, &lockedBuffer{})
	w := &projectWorld{c: c, env: env, sessions: map[authz.Role]map[string]string{}, userIDs: map[authz.Role]string{},
		monitorName: map[string]string{}, channelName: map[string]string{}, projectNames: map[string]string{}}
	owner := adminSession(c)
	w.sessions[authz.Owner] = owner
	w.userIDs[authz.Owner] = userIDOf(t, c, owner)
	for _, role := range []authz.Role{authz.Admin, authz.Member, authz.Viewer} {
		token, _ := invite(t, c, owner, string(role))
		w.sessions[role], _ = joinByRegister(t, c, token, "p-"+string(role)+"@example.com")
		w.userIDs[role] = userIDOf(t, c, w.sessions[role])
	}
	list := c.do(http.MethodGet, "/api/v1/projects", "", owner)
	items, _ := list.body["items"].([]any)
	if list.code != http.StatusOK || len(items) != 1 {
		t.Fatalf("initial projects: %d %s", list.code, list.raw)
	}
	w.def = items[0].(map[string]any)["id"].(string)
	w.projectNames[w.def] = "Default"
	mk := func(name string) string {
		r := c.do(http.MethodPost, "/api/v1/projects", `{"name":"`+name+`"}`, owner)
		if r.code != http.StatusCreated {
			t.Fatalf("create project %s: %d %s", name, r.code, r.raw)
		}
		id := r.body["id"].(string)
		w.projectNames[id] = name
		return id
	}
	w.alpha, w.beta, w.gamma = mk("Alpha"), mk("Beta"), mk("Gamma")
	put := func(project string, role authz.Role, body string) {
		r := c.do(http.MethodPut, "/api/v1/projects/"+project+"/members/"+w.userIDs[role], body, owner)
		if r.code != http.StatusOK {
			t.Fatalf("put %s in %s: %d %s", role, project, r.code, r.raw)
		}
	}
	put(w.alpha, authz.Member, `{}`)
	put(w.alpha, authz.Viewer, `{}`)
	put(w.beta, authz.Member, `{"role":"viewer"}`)

	monitor := func(project, name, extra string) string {
		r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"`+name+`","projectId":"`+project+`"`+extra+`}`, owner)
		if r.code != http.StatusCreated || r.body["projectId"] != project {
			t.Fatalf("create monitor %s: %d %s", name, r.code, r.raw)
		}
		id := r.body["id"].(string)
		w.monitorName[id] = name
		return id
	}
	w.mDef = monitor(w.def, "m-default", `,"url":"https://example.com/d"`)
	w.mAlpha = monitor(w.alpha, "m-alpha", `,"url":"https://example.com/a"`)
	w.mBeta = monitor(w.beta, "m-beta", `,"url":"https://example.com/b"`)
	w.mGam = monitor(w.gamma, "m-gamma", `,"url":"https://example.com/g"`)
	w.hbGamma = monitor(w.gamma, "hb-gamma", `,"type":"heartbeat"`)

	channel := func(name, scope string) string {
		r := c.do(http.MethodPost, "/api/v1/channels", `{"name":"`+name+`","webhookUrl":"https://hooks.slack.com/services/T000/B000/`+name+`"`+scope+`}`, owner)
		if r.code != http.StatusCreated {
			t.Fatalf("create channel %s: %d %s", name, r.code, r.raw)
		}
		id := r.body["id"].(string)
		w.channelName[id] = name
		return id
	}
	w.chDef = channel("c-default", "")
	w.chAlpha = channel("c-alpha", `,"projectId":"`+w.alpha+`"`)
	w.chBeta = channel("c-beta", `,"projectId":"`+w.beta+`"`)
	w.chGamma = channel("c-gamma", `,"projectId":"`+w.gamma+`"`)
	w.chOrg = channel("c-org", `,"organizationWide":true`)
	return w
}

func (w *projectWorld) names(t *testing.T, path string, hdr map[string]string, field string, dict map[string]string) string {
	t.Helper()
	r := w.c.do(http.MethodGet, path, "", hdr)
	if r.code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, r.code, r.raw)
	}
	items, _ := r.body["items"].([]any)
	var out []string
	for _, it := range items {
		id := it.(map[string]any)[field].(string)
		n, ok := dict[id]
		if !ok {
			n = id
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestIntegrationProjectIsolation is the project matrix of R-212: for every role, every endpoint
// that touches project-scoped data shows only the projects the role sees, answers 404 for the
// others (no IDOR inside one organization) and refuses changes where the role is read-only.
func TestIntegrationProjectIsolation(t *testing.T) {
	w := newProjectWorld(t, "projectiso")
	c := w.c

	// What each role sees in lists.
	allMonitors := "hb-gamma,m-alpha,m-beta,m-default,m-gamma"
	for role, want := range map[authz.Role][3]string{
		authz.Owner:  {"Alpha,Beta,Default,Gamma", allMonitors, "c-alpha,c-beta,c-default,c-gamma,c-org"},
		authz.Admin:  {"Alpha,Beta,Default,Gamma", allMonitors, "c-alpha,c-beta,c-default,c-gamma,c-org"},
		authz.Member: {"Alpha,Beta,Default", "m-alpha,m-beta,m-default", "c-alpha,c-beta,c-default,c-org"},
		authz.Viewer: {"Alpha,Default", "m-alpha,m-default", "c-alpha,c-default,c-org"},
	} {
		hdr := w.sessions[role]
		if got := w.names(t, "/api/v1/projects", hdr, "id", w.projectNames); got != want[0] {
			t.Errorf("%s projects = %s, want %s", role, got, want[0])
		}
		if got := w.names(t, "/api/v1/monitors", hdr, "id", w.monitorName); got != want[1] {
			t.Errorf("%s monitors = %s, want %s", role, got, want[1])
		}
		if got := w.names(t, "/api/v1/channels", hdr, "id", w.channelName); got != want[2] {
			t.Errorf("%s channels = %s, want %s", role, got, want[2])
		}
	}
	// The monitor list summary counts only visible monitors.
	if r := c.do(http.MethodGet, "/api/v1/monitors", "", w.sessions[authz.Viewer]); fmt.Sprint(r.body["summary"]) != "map[down:0 paused:0 unknown:2 up:0]" {
		t.Errorf("viewer summary: %s", r.raw)
	}
	// Filters: a visible project narrows the list, an invisible one is 404 like an unknown one.
	member := w.sessions[authz.Member]
	if got := w.names(t, "/api/v1/monitors?projectId="+w.alpha, member, "id", w.monitorName); got != "m-alpha" {
		t.Errorf("member filter alpha = %s", got)
	}
	if got := w.names(t, "/api/v1/channels?projectId="+w.alpha, member, "id", w.channelName); got != "c-alpha,c-org" {
		t.Errorf("member channel picker for alpha = %s", got)
	}
	if got := w.names(t, "/api/v1/monitors?projectId="+w.gamma, w.sessions[authz.Admin], "id", w.monitorName); got != "hb-gamma,m-gamma" {
		t.Errorf("admin filter gamma = %s", got)
	}

	// Every read and change by id of another project is 404 for a member and a viewer, exactly as
	// for an id that does not exist; owners and admins reach it.
	unknown := "0192b9c4-7c5e-7000-8000-0000000000ee"
	byID := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/monitors?projectId={p}", ""},
		{http.MethodGet, "/api/v1/channels?projectId={p}", ""},
		{http.MethodGet, "/api/v1/projects/{p}", ""},
		{http.MethodGet, "/api/v1/projects/{p}/members", ""},
		{http.MethodGet, "/api/v1/monitors/{m}", ""},
		{http.MethodGet, "/api/v1/monitors/{m}/stats", ""},
		{http.MethodGet, "/api/v1/monitors/{m}/checks", ""},
		{http.MethodGet, "/api/v1/monitors/{m}/incidents", ""},
		{http.MethodGet, "/api/v1/channels/{c}", ""},
	}
	writes := []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/monitors/{m}", `{"name":"stolen"}`},
		{http.MethodPost, "/api/v1/monitors/{m}/pause", ""},
		{http.MethodPost, "/api/v1/monitors/{m}/resume", ""},
		{http.MethodPost, "/api/v1/monitors/{hb}/heartbeat-token", ""},
		{http.MethodDelete, "/api/v1/monitors/{m}", ""},
	}
	fill := func(path, p, m, hb, ch string) string {
		return strings.NewReplacer("{p}", p, "{m}", m, "{hb}", hb, "{c}", ch).Replace(path)
	}
	for _, role := range []authz.Role{authz.Member, authz.Viewer} {
		for _, call := range byID {
			for _, ids := range [][4]string{{w.gamma, w.mGam, w.hbGamma, w.chGamma}, {unknown, unknown, unknown, unknown}} {
				r := c.do(call.method, fill(call.path, ids[0], ids[1], ids[2], ids[3]), call.body, w.sessions[role])
				if r.code != http.StatusNotFound || r.body["type"] != "urn:maiak:problem:not-found" {
					t.Errorf("%s %s %s: %d %s, want 404", role, call.method, fill(call.path, ids[0], ids[1], ids[2], ids[3]), r.code, r.raw)
				}
			}
		}
	}
	for _, call := range writes {
		r := c.do(call.method, fill(call.path, w.gamma, w.mGam, w.hbGamma, w.chGamma), call.body, member)
		if r.code != http.StatusNotFound {
			t.Errorf("member %s %s on gamma: %d %s, want 404", call.method, call.path, r.code, r.raw)
		}
		// Beta is read-only for the member (viewer override): 403, nothing changed.
		r = c.do(call.method, fill(call.path, w.beta, w.mBeta, w.mBeta, w.chBeta), call.body, member)
		if call.path == "/api/v1/monitors/{hb}/heartbeat-token" {
			continue
		}
		if r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:forbidden" {
			t.Errorf("member %s %s on beta: %d %s, want 403", call.method, call.path, r.code, r.raw)
		}
	}
	for _, role := range []authz.Role{authz.Owner, authz.Admin} {
		for _, call := range byID {
			r := c.do(call.method, fill(call.path, w.gamma, w.mGam, w.hbGamma, w.chGamma), call.body, w.sessions[role])
			if r.code != http.StatusOK {
				t.Errorf("%s %s on gamma: %d %s", role, call.path, r.code, r.raw)
			}
		}
	}
	if r := c.do(http.MethodGet, "/api/v1/monitors/"+w.mGam, "", w.sessions[authz.Owner]); r.body["name"] != "m-gamma" {
		t.Fatalf("gamma monitor changed by a member: %s", r.raw)
	}

	// Creates: the member creates in alpha, gets 403 in beta and a field error for gamma (it does
	// not see it); moving a monitor needs member rights in the target too.
	create := func(project string) response {
		return c.do(http.MethodPost, "/api/v1/monitors", `{"name":"by member","url":"https://example.com/x","projectId":"`+project+`"}`, member)
	}
	if r := create(w.alpha); r.code != http.StatusCreated {
		t.Errorf("member create in alpha: %d %s", r.code, r.raw)
	}
	if r := create(w.beta); r.code != http.StatusForbidden {
		t.Errorf("member create in beta: %d %s", r.code, r.raw)
	}
	if r := create(w.gamma); r.code != http.StatusUnprocessableEntity || !strings.Contains(r.raw, "projectId") {
		t.Errorf("member create in gamma: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/monitors/"+w.mAlpha, `{"projectId":"`+w.gamma+`"}`, member); r.code != http.StatusUnprocessableEntity {
		t.Errorf("member moves into gamma: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/monitors/"+w.mAlpha, `{"projectId":"`+w.beta+`"}`, member); r.code != http.StatusForbidden {
		t.Errorf("member moves into read-only beta: %d %s", r.code, r.raw)
	}
	// A member cannot select a channel of a project it does not see (it does not exist for it).
	if r := c.do(http.MethodPatch, "/api/v1/monitors/"+w.mAlpha, `{"channelIds":["`+w.chGamma+`"]}`, member); r.code != http.StatusUnprocessableEntity {
		t.Errorf("member selects gamma's channel: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/monitors/"+w.mAlpha, `{"channelIds":["`+w.chAlpha+`","`+w.chOrg+`"]}`, member); r.code != http.StatusOK {
		t.Errorf("member selects alpha and org channels: %d %s", r.code, r.raw)
	}

	// Usage stays per organization (quotas are shared by all projects).
	for role, hdr := range w.sessions {
		r := c.do(http.MethodGet, "/api/v1/usage", "", hdr)
		u, _ := r.body["usage"].(map[string]any)
		if r.code != http.StatusOK || u["monitors"] != float64(5) || u["heartbeats"] != float64(1) || u["projects"] != float64(4) {
			t.Errorf("%s usage: %d %s", role, r.code, r.raw)
		}
	}

	// Removing the member from alpha takes effect on its next request.
	owner := w.sessions[authz.Owner]
	if r := c.do(http.MethodDelete, "/api/v1/projects/"+w.alpha+"/members/"+w.userIDs[authz.Member], "", owner); r.code != http.StatusNoContent {
		t.Fatalf("remove member from alpha: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodGet, "/api/v1/monitors/"+w.mAlpha, "", member); r.code != http.StatusNotFound {
		t.Errorf("removed member still reads alpha: %d %s", r.code, r.raw)
	}
}

// TestIntegrationProjectsLifecycle covers projects CRUD, members, the project cap, and how
// channels follow moves and deletes.
func TestIntegrationProjectsLifecycle(t *testing.T) {
	w := newProjectWorld(t, "projectlife")
	c := w.c
	owner := w.sessions[authz.Owner]
	admin := w.sessions[authz.Admin]

	// Slugs: derived, suffixed when taken, explicit ones must be free.
	r := c.do(http.MethodPost, "/api/v1/projects", `{"name":"  Alpha  "}`, admin)
	if r.code != http.StatusCreated || r.body["slug"] != "alpha-2" || r.body["name"] != "Alpha" || r.body["role"] != "admin" {
		t.Fatalf("second Alpha: %d %s", r.code, r.raw)
	}
	second := r.body["id"].(string)
	if r := c.do(http.MethodPost, "/api/v1/projects", `{"name":"X","slug":"beta"}`, admin); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:slug-taken" {
		t.Fatalf("taken slug: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/projects/"+second, `{"name":"Alpha two","slug":"alpha-two"}`, admin); r.code != http.StatusOK || r.body["slug"] != "alpha-two" {
		t.Fatalf("rename: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/projects/"+second, `{"name":"   "}`, admin); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("blank name: %d %s", r.code, r.raw)
	}

	// Delete: the Default project never, a project with monitors or channels not, an empty one yes.
	if r := c.do(http.MethodDelete, "/api/v1/projects/"+w.def, "", owner); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:default-project" {
		t.Fatalf("delete Default: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodDelete, "/api/v1/projects/"+w.gamma, "", owner); r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:project-not-empty" {
		t.Fatalf("delete gamma: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodDelete, "/api/v1/projects/"+second, "", admin); r.code != http.StatusNoContent {
		t.Fatalf("delete empty: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodGet, "/api/v1/projects/"+second, "", owner); r.code != http.StatusNotFound {
		t.Fatalf("deleted project: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"x","url":"https://example.com/x","projectId":"`+second+`"}`, owner); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("monitor into a deleted project: %d %s", r.code, r.raw)
	}

	// Members of a project: owners and admins (all projects), explicit members, overrides.
	r = c.do(http.MethodGet, "/api/v1/projects/"+w.beta+"/members", "", owner)
	got := []string{}
	for _, it := range r.body["items"].([]any) {
		m := it.(map[string]any)
		got = append(got, fmt.Sprintf("%s:%s/%v/%s", m["organizationRole"], m["effectiveRole"], m["projectRole"], map[bool]string{true: "all", false: "listed"}[m["allProjects"] == true]))
	}
	slices.Sort(got)
	if strings.Join(got, " ") != "admin:admin/<nil>/all member:viewer/viewer/listed owner:owner/<nil>/all" {
		t.Fatalf("beta members: %v", got)
	}
	r = c.do(http.MethodGet, "/api/v1/projects/"+w.def+"/members", "", w.sessions[authz.Viewer])
	if items, _ := r.body["items"].([]any); r.code != http.StatusOK || len(items) != 4 {
		t.Fatalf("Default members (all members): %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPut, "/api/v1/projects/"+w.beta+"/members/0192b9c4-7c5e-7000-8000-0000000000ee", `{}`, owner); r.code != http.StatusNotFound {
		t.Fatalf("non-member into a project: %d %s", r.code, r.raw)
	}
	// An override never raises: a viewer with "member" stays a viewer.
	r = c.do(http.MethodPut, "/api/v1/projects/"+w.beta+"/members/"+w.userIDs[authz.Viewer], `{"role":"member"}`, owner)
	if r.code != http.StatusOK || r.body["effectiveRole"] != "viewer" {
		t.Fatalf("viewer with member override: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"x","url":"https://example.com/x","projectId":"`+w.beta+`"}`, w.sessions[authz.Viewer]); r.code != http.StatusForbidden {
		t.Fatalf("viewer creates: %d %s", r.code, r.raw)
	}

	// Channel moves and deletes keep selections inside the project. m-alpha selects c-alpha and
	// c-org; moving c-alpha to beta drops it from m-alpha.
	if r := c.do(http.MethodPatch, "/api/v1/monitors/"+w.mAlpha, `{"channelIds":["`+w.chAlpha+`","`+w.chOrg+`"]}`, owner); r.code != http.StatusOK {
		t.Fatalf("select: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/channels/"+w.chAlpha, `{"projectId":"`+w.beta+`","organizationWide":true}`, owner); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("both project and organization-wide: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/channels/"+w.chAlpha, `{"projectId":"`+w.beta+`"}`, owner); r.code != http.StatusOK || r.body["projectId"] != w.beta {
		t.Fatalf("move channel: %d %s", r.code, r.raw)
	}
	selected := func(monitor string) string {
		r := c.do(http.MethodGet, "/api/v1/monitors/"+monitor, "", owner)
		var out []string
		for _, id := range r.body["channelIds"].([]any) {
			out = append(out, w.channelName[id.(string)])
		}
		return strings.Join(out, ",")
	}
	if got := selected(w.mAlpha); got != "c-org" {
		t.Fatalf("after moving c-alpha: %s", got)
	}
	// Moving the monitor to gamma keeps the organization-wide channel; deleting it leaves the
	// monitor without a selection, which now means gamma's channels only (L-261).
	if r := c.do(http.MethodPatch, "/api/v1/monitors/"+w.mAlpha, `{"projectId":"`+w.gamma+`","channelIds":["`+w.chOrg+`","`+w.chGamma+`"]}`, owner); r.code != http.StatusOK || r.body["projectId"] != w.gamma {
		t.Fatalf("move monitor: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/channels/"+w.chOrg, `{"organizationWide":true}`, owner); r.code != http.StatusOK || r.body["projectId"] != nil {
		t.Fatalf("org-wide stays: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, "/api/v1/channels/"+w.chGamma, `{"organizationWide":true}`, owner); r.code != http.StatusOK || r.body["projectId"] != nil {
		t.Fatalf("make org-wide: %d %s", r.code, r.raw)
	}
	if got := selected(w.mAlpha); got != "c-gamma,c-org" && got != "c-org,c-gamma" {
		t.Fatalf("org-wide move keeps links: %s", got)
	}
	for _, ch := range []string{w.chOrg, w.chGamma} {
		if r := c.do(http.MethodDelete, "/api/v1/channels/"+ch, "", owner); r.code != http.StatusNoContent {
			t.Fatalf("delete channel: %d %s", r.code, r.raw)
		}
	}
	if got := selected(w.mAlpha); got != "" {
		t.Fatalf("after deletes: %s", got)
	}

	// The project cap (max_projects through the plans mechanism): 409 plan-limit-reached.
	r = c.do(http.MethodGet, "/api/v1/usage", "", owner)
	if l, _ := r.body["limits"].(map[string]any); l["projects"] != float64(50) {
		t.Fatalf("project limit: %s", r.raw)
	}
	for i := 0; i < 46; i++ {
		if r := c.do(http.MethodPost, "/api/v1/projects", fmt.Sprintf(`{"name":"P %d"}`, i), owner); r.code != http.StatusCreated {
			t.Fatalf("project %d: %d %s", i, r.code, r.raw)
		}
	}
	if r := c.do(http.MethodPost, "/api/v1/projects", `{"name":"one too many"}`, owner); r.code != http.StatusConflict ||
		r.body["type"] != "urn:maiak:problem:plan-limit-reached" || r.body["limit"] != "projects" || r.body["max"] != float64(50) {
		t.Fatalf("51st project: %d %s", r.code, r.raw)
	}
}

// TestIntegrationProjectMovesT13 covers the T-13 review fixes: the monitor detail shows only
// channels routing honours (a stray link of the previous release to another project's channel is
// hidden), and concurrent monitor and channel moves serialise (no deadlock, no stray link left).
func TestIntegrationProjectMovesT13(t *testing.T) {
	w := newProjectWorld(t, "projectmoves")
	c := w.c
	owner := w.sessions[authz.Owner]
	admin := w.env.AdminConn(t)

	pgtest.MustExec(t, admin, `INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id)
		SELECT tenant_id, '`+w.mAlpha+`', id FROM notification_channels WHERE id IN ('`+w.chBeta+`', '`+w.chOrg+`')`)
	r := c.do(http.MethodGet, "/api/v1/monitors/"+w.mAlpha, "", owner)
	if ids, _ := r.body["channelIds"].([]any); len(ids) != 1 || ids[0] != w.chOrg {
		t.Fatalf("detail channelIds with a stray link: %s", r.raw)
	}
	pgtest.MustExec(t, admin, `DELETE FROM monitor_channels WHERE monitor_id = '`+w.mAlpha+`'`)

	// m-alpha selects c-alpha; at the same time the monitor and the channel move. Whatever the
	// order, both succeed and no monitor keeps a channel of another project.
	for i := 0; i < 10; i++ {
		monProject, chProject := w.beta, w.gamma
		if i%2 == 1 {
			monProject, chProject = w.gamma, w.beta
		}
		if r := c.do(http.MethodPatch, "/api/v1/channels/"+w.chAlpha, `{"projectId":"`+w.alpha+`"}`, owner); r.code != http.StatusOK {
			t.Fatalf("reset channel: %d %s", r.code, r.raw)
		}
		if r := c.do(http.MethodPatch, "/api/v1/monitors/"+w.mAlpha, `{"projectId":"`+w.alpha+`","channelIds":["`+w.chAlpha+`"]}`, owner); r.code != http.StatusOK {
			t.Fatalf("reset monitor: %d %s", r.code, r.raw)
		}
		var wg sync.WaitGroup
		var codes [2]int
		var raws [2]string
		wg.Add(2)
		go func() {
			defer wg.Done()
			r := c.do(http.MethodPatch, "/api/v1/monitors/"+w.mAlpha, `{"projectId":"`+monProject+`"}`, owner)
			codes[0], raws[0] = r.code, r.raw
		}()
		go func() {
			defer wg.Done()
			r := c.do(http.MethodPatch, "/api/v1/channels/"+w.chAlpha, `{"projectId":"`+chProject+`"}`, owner)
			codes[1], raws[1] = r.code, r.raw
		}()
		wg.Wait()
		if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
			t.Fatalf("round %d: monitor move %d %s, channel move %d %s", i, codes[0], raws[0], codes[1], raws[1])
		}
		var stray int
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM monitor_channels mc
			JOIN monitors m ON m.id = mc.monitor_id JOIN notification_channels ch ON ch.id = mc.channel_id
			WHERE ch.project_id IS NOT NULL AND ch.project_id <> m.project_id`).Scan(&stray); err != nil || stray != 0 {
			t.Fatalf("round %d: %d stray links (%v)", i, stray, err)
		}
	}
}
