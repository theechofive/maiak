package migrations_test

import (
	"context"
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

// TestIntegrationProjectsContract checks the T-02 contract migration: monitors still without a
// project land in the Default project, stray links to another project's channel are deleted, valid
// links stay, the column is NOT NULL and the fill trigger is gone.
func TestIntegrationProjectsContract(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("projectscontract"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)

	pgtest.MustExec(t, admin, rollbackProjectsContract...)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A')`)
	var def string
	if err := admin.QueryRow(ctx, `SELECT id::text FROM projects WHERE tenant_id = $1 AND is_default`, tenantA).Scan(&def); err != nil {
		t.Fatal(err)
	}
	other := uuid.NewString()
	chanOther, chanOrg := uuid.NewString(), uuid.NewString()
	orphan, inDefault, inOther := uuid.NewString(), uuid.NewString(), uuid.NewString()
	const cfg = `'{"url":"https://example.com","expectedStatus":"2xx"}'`
	pgtest.MustExec(t, admin,
		`INSERT INTO projects (id, tenant_id, name, slug) VALUES ('`+other+`', '`+tenantA+`', 'Other', 'other')`,
		`INSERT INTO notification_channels (id, tenant_id, type, config_encrypted, project_id) VALUES
			('`+chanOther+`', '`+tenantA+`', 'slack_webhook', '\x01', '`+other+`'),
			('`+chanOrg+`', '`+tenantA+`', 'slack_webhook', '\x01', NULL)`,
		// A monitor the fill trigger missed (it cannot happen with the trigger on; the migration
		// still backfills defensively).
		`ALTER TABLE monitors DISABLE TRIGGER monitors_fill_project`,
		`INSERT INTO monitors (id, tenant_id, name, config, project_id) VALUES
			('`+orphan+`', '`+tenantA+`', 'orphan', `+cfg+`, NULL),
			('`+inDefault+`', '`+tenantA+`', 'default', `+cfg+`, '`+def+`'),
			('`+inOther+`', '`+tenantA+`', 'other', `+cfg+`, '`+other+`')`,
		`ALTER TABLE monitors ENABLE TRIGGER monitors_fill_project`,
		`INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES
			('`+tenantA+`', '`+inDefault+`', '`+chanOther+`'),
			('`+tenantA+`', '`+inDefault+`', '`+chanOrg+`'),
			('`+tenantA+`', '`+inOther+`', '`+chanOther+`')`,
	)

	if n, err := env.Migrate(ctx); err != nil || n != 1 {
		t.Fatalf("applied %d migrations, err=%v; want 1 (T-02 contract)", n, err)
	}

	var got string
	if err := admin.QueryRow(ctx, `SELECT project_id::text FROM monitors WHERE id = $1`, orphan).Scan(&got); err != nil || got != def {
		t.Fatalf("monitor without a project moved to %s (%v), want the Default project %s", got, err, def)
	}
	rows, err := admin.Query(ctx, `SELECT monitor_id::text || '>' || channel_id::text FROM monitor_channels WHERE tenant_id = $1 ORDER BY 1`, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	links, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{inDefault + ">" + chanOrg: true, inOther + ">" + chanOther: true}
	if len(links) != len(want) {
		t.Fatalf("links %v, want %v", links, want)
	}
	for _, l := range links {
		if !want[l] {
			t.Fatalf("unexpected link %s (all: %v)", l, links)
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
	err = inTenant(t, api, tenantA, func(tx pgx.Tx) error {
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
