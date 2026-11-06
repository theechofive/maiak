package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackStatusPages undoes the P-05 migration (ADR-0103), so a test can replay an older
// migration (goose refuses to apply a missing older version).
var rollbackStatusPages = []string{
	`DROP FUNCTION IF EXISTS status_page_snapshot(text)`,
	`DROP FUNCTION IF EXISTS status_page_published_pages()`,
	`DROP TABLE IF EXISTS status_page_snapshots`,
	`DROP TABLE IF EXISTS status_page_components`,
	`DROP TABLE IF EXISTS status_pages`,
	`UPDATE plans SET limits = limits - 'max_status_pages'`,
	`DELETE FROM goose_db_version WHERE version_id = 20261106120000`,
}

// TestIntegrationStatusPages checks the P-05 schema from the point of view of every runtime role:
// the public role reads one prepared snapshot by slug and nothing else, the snapshot function
// isolates tenants and answers nothing for unpublished, deleted and suspended pages alike, jobs
// lists published pages, and the constraints refuse reserved slugs and unsafe branding.
func TestIntegrationStatusPages(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("statuspages"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	worker := env.Conn(t, env.WorkerURL)
	public := env.Conn(t, env.StatuspageURL)

	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	pageA, pageB, pageHidden := uuid.NewString(), uuid.NewString(), uuid.NewString()
	pgtest.MustExec(t, admin,
		`INSERT INTO status_pages (id, tenant_id, slug, title, published, password_hash) VALUES
			('`+pageA+`', '`+tenantA+`', 'acme', 'Acme', true, NULL),
			('`+pageB+`', '`+tenantB+`', 'beta', 'Beta', true, '$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA'),
			('`+pageHidden+`', '`+tenantA+`', 'hidden', 'Hidden', false, NULL)`,
		`INSERT INTO status_page_snapshots (page_id, tenant_id, slug, payload, etag, generated_at) VALUES
			('`+pageA+`', '`+tenantA+`', 'acme', '{"status":"operational"}', '0123456789abcdef0123456789abcdef', now()),
			('`+pageB+`', '`+tenantB+`', 'beta', '{"status":"degraded"}', 'fedcba9876543210fedcba9876543210', now()),
			('`+pageHidden+`', '`+tenantA+`', 'hidden', '{"status":"operational"}', '00000000000000000000000000000000', now())`)

	snapshot := func(slug string) (payload string, hash *string, found bool) {
		t.Helper()
		err := public.QueryRow(ctx, `SELECT payload::text, password_hash FROM status_page_snapshot($1)`, slug).Scan(&payload, &hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false
		}
		if err != nil {
			t.Fatalf("status_page_snapshot(%s): %v", slug, err)
		}
		return payload, hash, true
	}

	t.Run("public role reads its own page only by slug", func(t *testing.T) {
		if p, hash, ok := snapshot("acme"); !ok || p != `{"status": "operational"}` || hash != nil {
			t.Fatalf("acme: %q %v %v", p, hash, ok)
		}
		if p, hash, ok := snapshot("beta"); !ok || p != `{"status": "degraded"}` || hash == nil {
			t.Fatalf("beta (protected): %q %v %v", p, hash, ok)
		}
		for _, slug := range []string{"hidden", "nope", "", "ACME", "acme' OR '1'='1"} {
			if _, _, ok := snapshot(slug); ok {
				t.Errorf("%q must not be found", slug)
			}
		}
	})

	t.Run("unpublished, deleted, suspended and stale slug look the same", func(t *testing.T) {
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantB+`'`)
		if _, _, ok := snapshot("beta"); ok {
			t.Error("page of a suspended tenant is served")
		}
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'active' WHERE id = '`+tenantB+`'`,
			`UPDATE status_pages SET slug = 'beta-new' WHERE id = '`+pageB+`'`)
		for _, slug := range []string{"beta", "beta-new"} {
			if _, _, ok := snapshot(slug); ok {
				t.Errorf("snapshot of an older slug served as %q", slug)
			}
		}
		pgtest.MustExec(t, admin, `UPDATE status_pages SET slug = 'beta', published = false, deleted_at = now() WHERE id = '`+pageB+`'`)
		if _, _, ok := snapshot("beta"); ok {
			t.Error("deleted page is served")
		}
	})

	t.Run("public role has no other privilege", func(t *testing.T) {
		var tables []string
		rows, err := admin.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
			WHERE c.relkind IN ('r', 'p', 'v') AND (has_table_privilege('maiak_statuspage', c.oid, 'SELECT')
			   OR has_table_privilege('maiak_statuspage', c.oid, 'INSERT') OR has_table_privilege('maiak_statuspage', c.oid, 'UPDATE')
			   OR has_table_privilege('maiak_statuspage', c.oid, 'DELETE'))`)
		if err != nil {
			t.Fatal(err)
		}
		if tables, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(tables) > 0 {
			t.Fatalf("maiak_statuspage has privileges on %v (%v)", tables, err)
		}
		var fns []string
		rows, err = admin.Query(ctx, `SELECT p.proname::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'public'
			WHERE has_function_privilege('maiak_statuspage', p.oid, 'EXECUTE') AND p.proowner = 'maiak_owner'::regrole
			  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e') ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		if fns, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(fns) != 1 || fns[0] != "status_page_snapshot" {
			t.Fatalf("maiak_statuspage may execute %v (%v), want only status_page_snapshot", fns, err)
		}
		for _, sql := range []string{
			`SELECT count(*) FROM status_page_snapshots`,
			`SELECT count(*) FROM status_pages`,
			`SELECT count(*) FROM monitors`,
			`SELECT * FROM status_page_published_pages()`,
		} {
			_, err := public.Exec(ctx, sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Errorf("%s as maiak_statuspage: err=%v, want 42501", sql, err)
			}
		}
		var readOnly string
		if err := public.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil || readOnly != "on" {
			t.Errorf("default_transaction_read_only=%s (%v)", readOnly, err)
		}
	})

	t.Run("jobs lists published pages of active tenants", func(t *testing.T) {
		rows, err := worker.Query(ctx, `SELECT page_id::text FROM status_page_published_pages()`)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) != 1 || ids[0] != pageA {
			t.Fatalf("published pages = %v (%v), want only %s", ids, err, pageA)
		}
		if _, err := api.Exec(ctx, `SELECT * FROM status_page_published_pages()`); err == nil {
			t.Error("maiak_api may list published pages")
		}
		if _, err := worker.Exec(ctx, `SELECT * FROM status_page_snapshot('acme')`); err == nil {
			t.Error("maiak_worker may read snapshots by slug")
		}
	})

	t.Run("tenant isolation and runtime grants", func(t *testing.T) {
		// RLS: tenant B sees none of tenant A's rows.
		err := inTenant(t, api, tenantB, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM status_pages WHERE tenant_id = $1) + (SELECT count(*) FROM status_page_snapshots WHERE tenant_id = $1)`, tenantA).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("tenant B sees %d rows of tenant A", n)
			}
			_, err := tx.Exec(ctx, `INSERT INTO status_pages (id, tenant_id, slug, title) VALUES ($1, $2, 'sneaky', 'x')`, uuid.NewString(), tenantA)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("cross-tenant insert: %v, want an RLS violation", err)
		}
		// jobs may write snapshots and delete components, nothing else.
		err = inTenant(t, worker, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE status_pages SET title = 'x' WHERE id = $1`, pageA)
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("worker update of status_pages: %v, want 42501", err)
		}
		err = inTenant(t, worker, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE status_page_snapshots SET generated_at = now() WHERE page_id = $1`, pageA)
			return err
		})
		if err != nil {
			t.Errorf("worker snapshot update: %v", err)
		}
		// The api soft-deletes pages.
		err = inTenant(t, api, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM status_pages WHERE id = $1`, pageHidden)
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("api delete of status_pages: %v, want 42501", err)
		}
	})

	t.Run("constraints", func(t *testing.T) {
		for name, sql := range map[string]string{
			"reserved slug":   `INSERT INTO status_pages (id, tenant_id, slug, title) VALUES (gen_random_uuid(), '` + tenantA + `', 'admin', 'x')`,
			"short slug":      `INSERT INTO status_pages (id, tenant_id, slug, title) VALUES (gen_random_uuid(), '` + tenantA + `', 'ab', 'x')`,
			"duplicate slug":  `INSERT INTO status_pages (id, tenant_id, slug, title) VALUES (gen_random_uuid(), '` + tenantB + `', 'acme', 'x')`,
			"http logo":       `INSERT INTO status_pages (id, tenant_id, slug, title, logo_url) VALUES (gen_random_uuid(), '` + tenantA + `', 'logo', 'x', 'http://example.com/a.png')`,
			"js logo":         `INSERT INTO status_pages (id, tenant_id, slug, title, logo_url) VALUES (gen_random_uuid(), '` + tenantA + `', 'logo', 'x', 'javascript:alert(1)')`,
			"upper accent":    `INSERT INTO status_pages (id, tenant_id, slug, title, accent_color) VALUES (gen_random_uuid(), '` + tenantA + `', 'accent', 'x', '#FF0000')`,
			"css accent":      `INSERT INTO status_pages (id, tenant_id, slug, title, accent_color) VALUES (gen_random_uuid(), '` + tenantA + `', 'accent', 'x', 'red;}')`,
			"plain password":  `INSERT INTO status_pages (id, tenant_id, slug, title, password_hash) VALUES (gen_random_uuid(), '` + tenantA + `', 'pw', 'x', 'secret')`,
			"published+gone":  `UPDATE status_pages SET deleted_at = now() WHERE id = '` + pageA + `'`,
			"foreign monitor": `INSERT INTO status_page_components (id, tenant_id, page_id, monitor_id, public_name, position) VALUES (gen_random_uuid(), '` + tenantA + `', '` + pageA + `', '` + seedMonitor(t, admin, tenantB, "now()", false, false) + `', 'x', 0)`,
		} {
			if _, err := admin.Exec(ctx, sql); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})

	t.Run("plan key", func(t *testing.T) {
		var limits string
		if err := admin.QueryRow(ctx, `SELECT string_agg(key || '=' || (limits->>'max_status_pages'), ',' ORDER BY key) FROM plans`).Scan(&limits); err != nil || limits != "free=1,internal=1" {
			t.Fatalf("max_status_pages = %s (%v)", limits, err)
		}
	})
}
