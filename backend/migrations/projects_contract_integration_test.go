package migrations_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackProjectsContract undoes the T-02 contract migration (L-285, O-03): the column is
// nullable again and the fill trigger of the expand step is back.
var rollbackProjectsContract = []string{
	`CREATE OR REPLACE FUNCTION monitors_fill_project() RETURNS trigger LANGUAGE plpgsql SET search_path = public, pg_temp AS $$
	BEGIN
		SELECT p.id INTO NEW.project_id FROM projects AS p WHERE p.tenant_id = NEW.tenant_id AND p.is_default;
		RETURN NEW;
	END
	$$`,
	`ALTER FUNCTION monitors_fill_project() OWNER TO maiak_owner`,
	// A replay after a failed older migration finds no project_id at all (rolled back further).
	`DO $$ BEGIN
		IF EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'monitors'::regclass AND attname = 'project_id' AND NOT attisdropped) THEN
			ALTER TABLE monitors ALTER COLUMN project_id DROP NOT NULL;
			DROP TRIGGER IF EXISTS monitors_fill_project ON monitors;
			CREATE TRIGGER monitors_fill_project BEFORE INSERT ON monitors
				FOR EACH ROW WHEN (NEW.project_id IS NULL) EXECUTE FUNCTION monitors_fill_project();
		END IF;
	END $$`,
	`DELETE FROM goose_db_version WHERE version_id = 20261106200000`,
}

// TestIntegrationProjectsContract checks the T-02 contract migration: it fails as a whole when a
// tenant has no Default project; otherwise monitors still without a project land in their own
// tenant's Default project, stray links to another project's channel are deleted, valid links
// stay, the column is NOT NULL and the fill trigger is gone.
func TestIntegrationProjectsContract(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("projectscontract"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)

	pgtest.MustExec(t, admin, rollbackRevokeTenantInsert...)
	pgtest.MustExec(t, admin, rollbackSchemaVersionGuard...)
	pgtest.MustExec(t, admin, rollbackProjectsContract...)
	const cfg = `'{"url":"https://example.com","expectedStatus":"2xx"}'`
	// seed gives tenant an Other project and, with the fill trigger off (it cannot miss a
	// monitor with the trigger on; the migration backfills defensively), a monitor without a
	// project, one in the Default project with a stray link to Other's channel and a valid link
	// to an organization-wide channel, and one in Other with a valid link. It returns the
	// orphan, the Default project and the links the migration must keep.
	seed := func(tenant, slug string) (orphan, def string, keep []string) {
		t.Helper()
		pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenant+`', '`+slug+`', '`+slug+`')`)
		if err := admin.QueryRow(ctx, `SELECT id::text FROM projects WHERE tenant_id = $1 AND is_default`, tenant).Scan(&def); err != nil {
			t.Fatal(err)
		}
		other, chanOther, chanOrg := uuid.NewString(), uuid.NewString(), uuid.NewString()
		orphan = uuid.NewString()
		inDefault, inOther := uuid.NewString(), uuid.NewString()
		pgtest.MustExec(t, admin,
			`INSERT INTO projects (id, tenant_id, name, slug) VALUES ('`+other+`', '`+tenant+`', 'Other', 'other')`,
			`INSERT INTO notification_channels (id, tenant_id, type, config_encrypted, project_id) VALUES
				('`+chanOther+`', '`+tenant+`', 'slack_webhook', '\x01', '`+other+`'),
				('`+chanOrg+`', '`+tenant+`', 'slack_webhook', '\x01', NULL)`,
			`ALTER TABLE monitors DISABLE TRIGGER monitors_fill_project`,
			`INSERT INTO monitors (id, tenant_id, name, config, project_id) VALUES
				('`+orphan+`', '`+tenant+`', 'orphan', `+cfg+`, NULL),
				('`+inDefault+`', '`+tenant+`', 'default', `+cfg+`, '`+def+`'),
				('`+inOther+`', '`+tenant+`', 'other', `+cfg+`, '`+other+`')`,
			`ALTER TABLE monitors ENABLE TRIGGER monitors_fill_project`,
			`INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES
				('`+tenant+`', '`+inDefault+`', '`+chanOther+`'),
				('`+tenant+`', '`+inDefault+`', '`+chanOrg+`'),
				('`+tenant+`', '`+inOther+`', '`+chanOther+`')`,
		)
		return orphan, def, []string{inDefault + ">" + chanOrg, inOther + ">" + chanOther}
	}
	orphanA, defA, keepA := seed(tenantA, "a")
	orphanB, defB, keepB := seed(tenantB, "b")
	projectOf := func(monitor string) string {
		t.Helper()
		var p *string
		if err := admin.QueryRow(ctx, `SELECT project_id::text FROM monitors WHERE id = $1`, monitor).Scan(&p); err != nil {
			t.Fatal(err)
		}
		if p == nil {
			return "NULL"
		}
		return *p
	}
	links := func(tenant string) []string {
		t.Helper()
		rows, err := admin.Query(ctx, `SELECT monitor_id::text || '>' || channel_id::text FROM monitor_channels WHERE tenant_id = $1 ORDER BY 1`, tenant)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// A tenant without a Default project leaves a monitor without one: the migration fails as a
	// whole and changes nothing.
	tenantC := uuid.NewString()
	orphanC := uuid.NewString()
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantC+`', 'c', 'C')`,
		`DELETE FROM projects WHERE tenant_id = '`+tenantC+`'`,
		`INSERT INTO monitors (id, tenant_id, name, config) VALUES ('`+orphanC+`', '`+tenantC+`', 'orphan', `+cfg+`)`)
	if n, err := env.Migrate(ctx); err == nil || n != 0 {
		t.Fatalf("migration over a tenant without a Default project: applied %d, err=%v; want a failure", n, err)
	}
	var recorded int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id = 20261106200000`).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("failed contract recorded %d times (%v)", recorded, err)
	}
	if projectOf(orphanA) != "NULL" || len(links(tenantA)) != 3 || len(links(tenantB)) != 3 {
		t.Fatalf("failed migration changed data: orphan A in %s, links A %v, links B %v", projectOf(orphanA), links(tenantA), links(tenantB))
	}
	var fill bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'monitors'::regclass AND tgname = 'monitors_fill_project')`).Scan(&fill); err != nil || !fill {
		t.Fatalf("fill trigger after the failed migration: %t (%v), want kept", fill, err)
	}
	pgtest.MustExec(t, admin, `DELETE FROM monitors WHERE id = '`+orphanC+`'`)

	if n, err := env.Migrate(ctx); err != nil || n != 3 {
		t.Fatalf("applied %d migrations, err=%v; want 3 (T-02 contract, O-06, O-07)", n, err)
	}
	for _, c := range []struct {
		orphan, def, tenant string
		keep                []string
	}{{orphanA, defA, tenantA, keepA}, {orphanB, defB, tenantB, keepB}} {
		if got := projectOf(c.orphan); got != c.def {
			t.Fatalf("monitor without a project of %s moved to %s, want its Default project %s", c.tenant, got, c.def)
		}
		got := links(c.tenant)
		want := append([]string(nil), c.keep...)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("links of %s: %v, want %v (stray link deleted, valid ones kept)", c.tenant, got, want)
		}
	}

	var notNull, trigger, function bool
	if err := admin.QueryRow(ctx, `SELECT
			(SELECT attnotnull FROM pg_attribute WHERE attrelid = 'monitors'::regclass AND attname = 'project_id'),
			EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'monitors'::regclass AND tgname = 'monitors_fill_project'),
			to_regprocedure('monitors_fill_project()') IS NOT NULL`).Scan(&notNull, &trigger, &function); err != nil {
		t.Fatal(err)
	}
	if !notNull || trigger || function {
		t.Fatalf("project_id NOT NULL=%t, fill trigger=%t, fill function=%t; want true, false, false", notNull, trigger, function)
	}

	// A monitor without a project (an api from before T-02) is refused now.
	err := inTenant(t, api, tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO monitors (id, tenant_id, name, config) VALUES (gen_random_uuid(), $1, 'old api', `+cfg+`)`, tenantA)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not-null") && !strings.Contains(err.Error(), "null value") {
		t.Fatalf("monitor without a project accepted: %v", err)
	}
	// New tenants still get their Default project from the database.
	newID := uuid.New()
	if _, err := api.Exec(ctx, `SELECT auth_create_tenant($1, 'fresh', 'Fresh')`, newID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM projects WHERE tenant_id = $1 AND is_default`, newID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("default projects of a new tenant: %d (%v), want 1", n, err)
	}
}
