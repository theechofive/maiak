package public_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/statuspages"
	"github.com/theechofive/maiak/backend/internal/statuspages/public"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationRendererOverTheDatabase runs the renderer as maiak_statuspage against pages the
// admin service created: a published page renders, its JSON matches; an unpublished page, a
// deleted page and an unknown slug get byte-identical 404s; a protected page shows the lock form.
func TestIntegrationRendererOverTheDatabase(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("statuspagerender"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	tenant, user := uuid.New(), uuid.New()
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenant.String()+`', 'render', 'R')`,
		`INSERT INTO users (id, email) VALUES ('`+user.String()+`', 'render@example.com')`)
	svc := statuspages.NewService(env.Pool(t, env.APIURL, 2), statuspages.Options{HashParams: &crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}})
	a := statuspages.Actor{TenantID: tenant, Access: authz.Actor{UserID: user, Role: authz.Admin}}
	page, err := svc.Create(ctx, a, statuspages.CreateInput{Slug: "render-me", Title: "Render <b>me</b>"})
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false

	srv := httptest.NewServer(public.NewHandler(public.Config{PublicURL: "https://maiak.example.com", TrustedProxyHops: 0},
		public.Deps{Source: public.DBSource{Pool: env.Pool(t, env.StatuspageURL, 2)}, Cookies: public.NewCookieSigner(make([]byte, 32))}))
	t.Cleanup(srv.Close)
	get := func(path string) (int, string) {
		t.Helper()
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}

	unknownCode, unknownBody := get("/status/never-existed")
	if code, body := get("/status/render-me"); code != unknownCode || body != unknownBody || code != http.StatusNotFound {
		t.Fatalf("unpublished page: %d", code)
	}
	// New renderer instances (no cache) for the changed states.
	fresh := func() *httptest.Server {
		s := httptest.NewServer(public.NewHandler(public.Config{PublicURL: "https://maiak.example.com"},
			public.Deps{Source: public.DBSource{Pool: env.Pool(t, env.StatuspageURL, 2)}, Cookies: public.NewCookieSigner(make([]byte, 32))}))
		t.Cleanup(s.Close)
		return s
	}
	if _, err := svc.Update(ctx, a, page.ID, statuspages.UpdateInput{Published: &yes}); err != nil {
		t.Fatal(err)
	}
	// The renderer caches for 15 s (the 404 above too); a fresh handler stands for the next miss.
	srv = fresh()
	code, body := get("/status/render-me")
	if code != http.StatusOK || !strings.Contains(body, "Render &lt;b&gt;me&lt;/b&gt;") || !strings.Contains(body, "No components.") {
		t.Fatalf("published page: %d\n%s", code, body)
	}
	if code, body := get("/status/render-me.json"); code != http.StatusOK || !strings.Contains(body, `"slug":"render-me"`) {
		t.Fatalf("json: %d %s", code, body)
	}

	pw := "a long enough password"
	if err := svc.SetPassword(ctx, a, page.ID, &pw); err != nil {
		t.Fatal(err)
	}
	srv = fresh()
	if code, body := get("/status/render-me"); code != http.StatusOK || !strings.Contains(body, "This status page is protected") {
		t.Fatalf("protected: %d", code)
	}
	if code, body := get("/status/render-me.json"); code != unknownCode || body != unknownBody {
		t.Fatalf("json of a protected page: %d", code)
	}
	if _, err := svc.Update(ctx, a, page.ID, statuspages.UpdateInput{Published: &no}); err != nil {
		t.Fatal(err)
	}
	srv = fresh()
	if code, body := get("/status/render-me"); code != unknownCode || body != unknownBody {
		t.Fatalf("unpublished again: %d", code)
	}
	if err := svc.Delete(ctx, a, page.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := get("/status/render-me"); code != unknownCode || body != unknownBody {
		t.Fatalf("deleted: %d", code)
	}
}
