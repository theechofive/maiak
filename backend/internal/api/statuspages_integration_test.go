package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/authz"
)

// TestIntegrationStatusPageAdmin covers the admin API of ADR-0103: roles, validation, the global
// slug and the plan limit, components from several projects, the preview projection, publishing
// (the snapshot appears and disappears for the public role), the write-only password and audit rows.
func TestIntegrationStatusPageAdmin(t *testing.T) {
	w := newProjectWorld(t, "apistatuspages")
	c, owner := w.c, w.sessions[authz.Owner]
	ctx := context.Background()
	public := w.env.Conn(t, w.env.StatuspageURL)
	snapshot := func(slug string) (payload string, hash *string, found bool) {
		t.Helper()
		err := public.QueryRow(ctx, `SELECT payload::text, password_hash FROM status_page_snapshot($1)`, slug).Scan(&payload, &hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return payload, hash, true
	}

	// Validation: reserved slug, bad logo, bad accent.
	r := c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"status","title":"x","logoUrl":"http://x.example/a.png","accentColor":"red"}`, owner)
	if r.code != http.StatusUnprocessableEntity || !strings.Contains(r.raw, `"slug"`) || !strings.Contains(r.raw, `"logoUrl"`) || !strings.Contains(r.raw, `"accentColor"`) {
		t.Fatalf("invalid create: %d %s", r.code, r.raw)
	}
	for _, role := range []authz.Role{authz.Member, authz.Viewer} {
		if r := c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"acme-status","title":"Acme"}`, w.sessions[role]); r.code != http.StatusForbidden {
			t.Fatalf("create as %s: %d %s", role, r.code, r.raw)
		}
	}
	r = c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"acme-status","title":"Acme","logoUrl":"https://cdn.example.com/logo.png","accentColor":"#1F6FEB"}`, w.sessions[authz.Admin])
	if r.code != http.StatusCreated || r.body["published"] != false || r.body["accentColor"] != "#1f6feb" ||
		r.body["publicUrl"] != "https://maiak.example.com/status/acme-status" || r.body["passwordProtected"] != false {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	id := r.body["id"].(string)
	base := "/api/v1/status-pages/" + id

	// One page per organization (plan), and slugs are global.
	if r := c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"second","title":"Second"}`, owner); r.code != http.StatusConflict ||
		r.body["type"] != "urn:maiak:problem:plan-limit-reached" || r.body["limit"] != "statusPages" {
		t.Fatalf("second page: %d %s", r.code, r.raw)
	}
	reg := c.do(http.MethodPost, "/api/v1/auth/register", registerBody("other-org@example.com", ownerPassword), map[string]string{"X-Forwarded-For": "198.51.100.40"})
	other, csrf := session(t, reg)
	other = withCSRF(other, csrf)
	if r := c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"acme-status","title":"Mine"}`, other); r.code != http.StatusConflict ||
		r.body["type"] != "urn:maiak:problem:slug-taken" || r.body["title"] != "This address is not available" {
		t.Fatalf("taken slug: %d %s", r.code, r.raw)
	}
	// Another organization cannot see or change this page.
	if r := c.do(http.MethodGet, base, "", other); r.code != http.StatusNotFound {
		t.Fatalf("foreign get: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPatch, base, `{"published":true}`, other); r.code != http.StatusNotFound {
		t.Fatalf("foreign publish: %d %s", r.code, r.raw)
	}

	// Every role reads the settings; only admins see the components.
	for _, role := range []authz.Role{authz.Viewer, authz.Member} {
		if r := c.do(http.MethodGet, "/api/v1/status-pages", "", w.sessions[role]); r.code != http.StatusOK || !strings.Contains(r.raw, "acme-status") {
			t.Fatalf("list as %s: %d %s", role, r.code, r.raw)
		}
		for _, call := range [][2]string{{http.MethodGet, base + "/components"}, {http.MethodGet, base + "/preview"}, {http.MethodPatch, base}} {
			if r := c.do(call[0], call[1], `{"published":true}`, w.sessions[role]); r.code != http.StatusForbidden {
				t.Fatalf("%s %s as %s: %d", call[0], call[1], role, r.code)
			}
		}
	}

	// Components from three projects, a heartbeat included; groups and order.
	body := `{"items":[
		{"monitorId":"` + w.mGam + `","publicName":"Payments API","groupName":"Core"},
		{"monitorId":"` + w.hbGamma + `","publicName":"Nightly billing","groupName":"Core"},
		{"monitorId":"` + w.mAlpha + `","publicName":"Website"}]}`
	r = c.do(http.MethodPut, base+"/components", body, owner)
	if r.code != http.StatusOK {
		t.Fatalf("put components: %d %s", r.code, r.raw)
	}
	items := r.body["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["publicName"] != "Payments API" || items[2].(map[string]any)["groupName"] != nil ||
		items[1].(map[string]any)["monitorType"] != "heartbeat" {
		t.Fatalf("components: %s", r.raw)
	}
	for _, bad := range []string{
		`{"items":[{"monitorId":"0192b9c4-7c5e-7000-8000-0000000000aa","publicName":"Ghost"}]}`,
		`{"items":[{"monitorId":"` + w.mGam + `","publicName":"A"},{"monitorId":"` + w.mGam + `","publicName":"B"}]}`,
		`{"items":[{"monitorId":"` + w.mGam + `","publicName":"   "}]}`,
	} {
		if r := c.do(http.MethodPut, base+"/components", bad, owner); r.code != http.StatusUnprocessableEntity || !strings.Contains(r.raw, "items.") {
			t.Fatalf("bad components %s: %d %s", bad, r.code, r.raw)
		}
	}

	// The preview is the public projection: public names only.
	r = c.do(http.MethodGet, base+"/preview", "", owner)
	if r.code != http.StatusOK || r.body["status"] == nil || len(r.body["components"].([]any)) != 3 {
		t.Fatalf("preview: %d %s", r.code, r.raw)
	}
	for _, secret := range []string{w.mGam, w.hbGamma, w.mAlpha, w.gamma, w.alpha, w.monitorName[w.mGam], w.monitorName[w.mAlpha], "https://example.com/", "Gamma", "Alpha", id} {
		if secret != "" && strings.Contains(r.raw, secret) {
			t.Fatalf("preview leaks %q: %s", secret, r.raw)
		}
	}
	days := r.body["components"].([]any)[0].(map[string]any)["days"].([]any)
	if len(days) != 90 {
		t.Fatalf("days: %d", len(days))
	}
	if _, _, ok := snapshot("acme-status"); ok {
		t.Fatal("an unpublished page has a public snapshot")
	}

	// Publish: the snapshot exists at once, with the same keys as the preview.
	r = c.do(http.MethodPatch, base, `{"published":true}`, owner)
	if r.code != http.StatusOK || r.body["published"] != true || r.body["componentCount"] != float64(3) {
		t.Fatalf("publish: %d %s", r.code, r.raw)
	}
	payload, hash, ok := snapshot("acme-status")
	if !ok || hash != nil || !strings.Contains(payload, "Payments API") || strings.Contains(payload, w.mGam) {
		t.Fatalf("published snapshot: %v %q", ok, payload)
	}
	var snap map[string]any
	if err := json.Unmarshal([]byte(payload), &snap); err != nil || len(snap) != 7 {
		t.Fatalf("snapshot keys: %v (%v)", snap, err)
	}

	// Renaming the slug moves the public page; the old one is gone.
	if r := c.do(http.MethodPatch, base, `{"slug":"acme"}`, owner); r.code != http.StatusOK {
		t.Fatalf("rename: %d %s", r.code, r.raw)
	}
	if _, _, ok := snapshot("acme-status"); ok {
		t.Fatal("old slug still served")
	}
	if _, _, ok := snapshot("acme"); !ok {
		t.Fatal("new slug not served")
	}

	// Password: write-only, never returned; the renderer gets the hash.
	if r := c.do(http.MethodPut, base+"/password", `{"password":"short"}`, owner); r.code != http.StatusBadRequest && r.code != http.StatusUnprocessableEntity {
		t.Fatalf("short password: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPut, base+"/password", `{"password":"correct horse battery"}`, owner); r.code != http.StatusNoContent {
		t.Fatalf("set password: %d %s", r.code, r.raw)
	}
	r = c.do(http.MethodGet, base, "", w.sessions[authz.Viewer])
	if r.body["passwordProtected"] != true || strings.Contains(r.raw, "argon2") || strings.Contains(r.raw, "correct horse") {
		t.Fatalf("protected page: %s", r.raw)
	}
	if _, hash, _ := snapshot("acme"); hash == nil || !strings.HasPrefix(*hash, "$argon2id$") {
		t.Fatalf("hash for the renderer: %v", hash)
	}
	if r := c.do(http.MethodDelete, base+"/password", "", owner); r.code != http.StatusNoContent {
		t.Fatalf("clear password: %d", r.code)
	}
	if _, hash, _ := snapshot("acme"); hash != nil {
		t.Fatal("password not cleared")
	}

	// Clearing branding with empty strings.
	r = c.do(http.MethodPatch, base, `{"logoUrl":"","accentColor":""}`, owner)
	if r.code != http.StatusOK || r.body["logoUrl"] != nil || r.body["accentColor"] != nil {
		t.Fatalf("clear branding: %d %s", r.code, r.raw)
	}

	// A deleted monitor's component disappears.
	if r := c.do(http.MethodDelete, "/api/v1/monitors/"+w.mAlpha, "", owner); r.code != http.StatusNoContent {
		t.Fatalf("delete monitor: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodGet, base+"/components", "", owner); len(r.body["items"].([]any)) != 2 {
		t.Fatalf("components after monitor delete: %s", r.raw)
	}

	// Unpublish and delete: the public page goes away; the slug becomes free.
	if r := c.do(http.MethodPatch, base, `{"published":false}`, owner); r.code != http.StatusOK {
		t.Fatalf("unpublish: %d", r.code)
	}
	if _, _, ok := snapshot("acme"); ok {
		t.Fatal("unpublished page still served")
	}
	if r := c.do(http.MethodDelete, base, "", owner); r.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.code, r.raw)
	}
	// The freed address stays reserved for 30 days (P-06): another organization gets the same
	// answer as for a live page anywhere, which says nothing about whose it is; the old address of a
	// rename is held too. The organization itself may reclaim it.
	for _, slug := range []string{"acme", "acme-status"} {
		r := c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"`+slug+`","title":"Other org"}`, other)
		if r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:slug-taken" || r.body["title"] != "This address is not available" {
			t.Fatalf("held slug %s: %d %s", slug, r.code, r.raw)
		}
	}
	if r := c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"acme","title":"Back"}`, owner); r.code != http.StatusCreated {
		t.Fatalf("own slug not reclaimable: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"acme","title":"Taken"}`, other); r.code != http.StatusConflict ||
		r.body["title"] != "This address is not available" {
		t.Fatalf("live slug: %d %s", r.code, r.raw)
	}
	if r := c.do(http.MethodPost, "/api/v1/status-pages", `{"slug":"other-org","title":"Other org"}`, other); r.code != http.StatusCreated {
		t.Fatalf("free slug: %d %s", r.code, r.raw)
	}

	// Usage counts the page; audit rows carry field names, never the password.
	if r := c.do(http.MethodGet, "/api/v1/usage", "", other); r.body["usage"].(map[string]any)["statusPages"] != float64(1) ||
		r.body["limits"].(map[string]any)["statusPages"] != float64(1) {
		t.Fatalf("usage: %s", r.raw)
	}
	admin := w.env.AdminConn(t)
	var actions, meta string
	if err := admin.QueryRow(ctx, `SELECT string_agg(action, ',' ORDER BY created_at, id), string_agg(metadata::text, ' ')
		FROM audit_log WHERE entity_type = 'status_page' AND entity_id = $1`, id).Scan(&actions, &meta); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"status_page.created", "status_page.published", "status_page.updated", "status_page.unpublished", "status_page.deleted"} {
		if !strings.Contains(actions, want) {
			t.Errorf("audit lacks %s: %s", want, actions)
		}
	}
	if strings.Contains(meta, "correct horse") || strings.Contains(meta, "argon2") || strings.Contains(meta, "cdn.example.com") {
		t.Fatalf("audit metadata leaks values: %s", meta)
	}
}
