package migrations_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackProjects undoes the T-02 migration (ADR-0102), so a test can replay an older migration
// (goose refuses to apply a missing older version). enqueue_incident_reminders keeps the T-02 body
// until the replay re-applies the migrations after S-01.
var rollbackProjects = []string{
	`DROP TRIGGER IF EXISTS monitors_fill_project ON monitors`,
	`DROP FUNCTION IF EXISTS monitors_fill_project()`,
	`DROP TRIGGER IF EXISTS tenants_create_default_project ON tenants`,
	`DROP FUNCTION IF EXISTS tenants_create_default_project()`,
	`ALTER TABLE monitors DROP COLUMN IF EXISTS project_id`,
	`ALTER TABLE notification_channels DROP COLUMN IF EXISTS project_id`,
	`DROP TABLE IF EXISTS project_members`,
	`DROP TABLE IF EXISTS projects`,
	`DROP FUNCTION IF EXISTS projects_uuidv7()`,
	`UPDATE plans SET limits = limits - 'max_projects'`,
	`DELETE FROM goose_db_version WHERE version_id = 20261106090000`,
}

// TestIntegrationProjects checks the T-02 migration: the backfill over existing tenants, monitors
// and channels, the Default project of new tenants, isolation and grants of the new tables, and the
// routing rule of enqueue_incident_reminders.
func TestIntegrationProjects(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("projects"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	worker := env.Conn(t, env.WorkerURL)

	// The previous release's database with data: two tenants, monitors (one deleted), live and
	// deleted channels and a channel selection. Then the T-02 migration runs over it.
	pgtest.MustExec(t, admin, rollbackPlanFeaturesLastOrg...)
	pgtest.MustExec(t, admin, rollbackRevokeTenantInsert...)
	pgtest.MustExec(t, admin, rollbackSchemaVersionGuard...)
	pgtest.MustExec(t, admin, rollbackProjectsContract...)
	pgtest.MustExec(t, admin, rollbackOAuthLogin...)
	pgtest.MustExec(t, admin, rollbackStatusPages...)
	pgtest.MustExec(t, admin, rollbackProjects...)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	monA := seedMonitor(t, admin, tenantA, "now()", false, false)
	monGone := seedMonitor(t, admin, tenantA, "now()", false, true)
	monB := seedMonitor(t, admin, tenantB, "now()", false, false)
	chanA, chanGone := uuid.NewString(), uuid.NewString()
	pgtest.MustExec(t, admin,
		`INSERT INTO notification_channels (id, tenant_id, type, config_encrypted) VALUES ('`+chanA+`', '`+tenantA+`', 'slack_webhook', '\x01')`,
		`INSERT INTO notification_channels (id, tenant_id, type, deleted_at, status) VALUES ('`+chanGone+`', '`+tenantA+`', 'slack_webhook', now(), 'failed')`,
		`INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES ('`+tenantA+`', '`+monA+`', '`+chanA+`')`)
	if n, err := env.Migrate(ctx); err != nil || n != 8 {
		t.Fatalf("re-applied %d migrations, err=%v; want 8 (T-02, P-05, P-06, O-01, T-02 contract, O-06, O-07, O-08)", n, err)
	}

	defaultOf := func(tenant string) string {
		t.Helper()
		var id string
		if err := admin.QueryRow(ctx, `SELECT id::text FROM projects WHERE tenant_id = $1 AND is_default AND all_members AND slug = 'default' AND name = 'Default'`, tenant).Scan(&id); err != nil {
			t.Fatalf("default project of %s: %v", tenant, err)
		}
		return id
	}
	projectOf := func(table, id string) string {
		t.Helper()
		var p *string
		if err := admin.QueryRow(ctx, `SELECT project_id::text FROM `+table+` WHERE id = $1`, id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		if p == nil {
			return "NULL"
		}
		return *p
	}

	t.Run("backfill", func(t *testing.T) {
		defA, defB := defaultOf(tenantA), defaultOf(tenantB)
		for _, c := range []struct{ table, id, want string }{
			{"monitors", monA, defA}, {"monitors", monGone, defA}, {"monitors", monB, defB},
			{"notification_channels", chanA, defA}, {"notification_channels", chanGone, defA},
		} {
			if got := projectOf(c.table, c.id); got != c.want {
				t.Errorf("%s %s project = %s, want %s", c.table, c.id, got, c.want)
			}
		}
		var version int
		if err := admin.QueryRow(ctx, `SELECT get_byte(uuid_send($1::uuid), 6) >> 4`, defA).Scan(&version); err != nil || version != 7 {
			t.Fatalf("project id version = %d (%v), want 7", version, err)
		}
		var limits string
		if err := admin.QueryRow(ctx, `SELECT string_agg(key || '=' || (limits->>'max_projects'), ',' ORDER BY key) FROM plans`).Scan(&limits); err != nil || limits != "community=2147483647,free=50,internal=50" {
			t.Fatalf("max_projects = %s (%v)", limits, err)
		}
	})

	t.Run("new tenants and organization-wide channels", func(t *testing.T) {
		// auth_create_tenant (and a direct insert) gives the new tenant its Default project.
		var created uuid.UUID
		newID := uuid.New()
		if err := api.QueryRow(ctx, `SELECT auth_create_tenant($1, 'fresh', 'Fresh')`, newID).Scan(&created); err != nil || created != newID {
			t.Fatalf("auth_create_tenant: %v %v", created, err)
		}
		defaultOf(newID.String())
		// A channel without a project is organization-wide (by design, ADR-0102). Monitors without
		// a project are refused since the contract step (TestIntegrationProjectsContract).
		var ch string
		err := inTenant(t, api, newID.String(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted) VALUES (gen_random_uuid(), $1, 'slack_webhook', '\x01') RETURNING id::text`, newID).Scan(&ch)
		})
		if err != nil || projectOf("notification_channels", ch) != "NULL" {
			t.Fatalf("organization-wide channel: %v %s", err, projectOf("notification_channels", ch))
		}
	})

	t.Run("constraints and isolation", func(t *testing.T) {
		defA, defB := defaultOf(tenantA), defaultOf(tenantB)
		// A monitor cannot point at another tenant's project (composite key).
		err := inTenant(t, api, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE monitors SET project_id = $1 WHERE id = $2`, defB, monA)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "foreign key") {
			t.Fatalf("cross-tenant project accepted: %v", err)
		}
		// One Default project per tenant, unique live slugs, the Default project cannot be deleted.
		for _, stmt := range []string{
			`INSERT INTO projects (id, tenant_id, name, slug, is_default) VALUES (gen_random_uuid(), $1, 'Second', 'second', true)`,
			`INSERT INTO projects (id, tenant_id, name, slug) VALUES (gen_random_uuid(), $1, 'Dup', 'default')`,
			`UPDATE projects SET deleted_at = now() WHERE tenant_id = $1 AND is_default`,
			`INSERT INTO projects (id, tenant_id, name, slug) VALUES (gen_random_uuid(), $1, 'Bad', 'Bad Slug')`,
		} {
			err := inTenant(t, api, tenantA, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt, tenantA); return err })
			if err == nil {
				t.Errorf("accepted: %s", stmt)
			}
		}
		// A soft-deleted project frees its slug.
		err = inTenant(t, api, tenantA, func(tx pgx.Tx) error {
			for _, s := range []string{
				`INSERT INTO projects (id, tenant_id, name, slug, deleted_at) VALUES (gen_random_uuid(), $1, 'Old', 'web', now())`,
				`INSERT INTO projects (id, tenant_id, name, slug) VALUES (gen_random_uuid(), $1, 'Web', 'web')`,
			} {
				if _, err := tx.Exec(ctx, s, tenantA); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n := countInTenant(t, api, tenantA, `SELECT count(*) FROM projects WHERE tenant_id <> '`+tenantA+`'`); n != 0 {
			t.Fatalf("api sees %d projects of other tenants", n)
		}
		// The api cannot hard-delete projects; the worker has no access at all.
		err = inTenant(t, api, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = $1`, defA)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("api deleted a project: %v", err)
		}
		for _, table := range []string{"projects", "project_members"} {
			if _, err := worker.Exec(ctx, `SELECT count(*) FROM `+table); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("worker reads %s: %v", table, err)
			}
		}
		// project_members: role override member or viewer only, removed with the membership.
		user := uuid.NewString()
		pgtest.MustExec(t, admin, `INSERT INTO users (id, email) VALUES ('`+user+`', 'pm@example.com')`,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ('`+tenantA+`', '`+user+`', 'member')`)
		err = inTenant(t, api, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO project_members (tenant_id, project_id, user_id, role) VALUES ($1, $2, $3, 'admin')`, tenantA, defA, user)
			return err
		})
		if err == nil {
			t.Fatal("project role admin accepted")
		}
		err = inTenant(t, api, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO project_members (tenant_id, project_id, user_id, role) VALUES ($1, $2, $3, 'viewer')`, tenantA, defA, user)
			if err == nil {
				_, err = tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantA, user)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if n := countInTenant(t, api, tenantA, `SELECT count(*) FROM project_members`); n != 0 {
			t.Fatalf("%d project_members rows survive the membership", n)
		}
	})

	t.Run("reminders follow the project", func(t *testing.T) {
		defA := defaultOf(tenantA)
		other := uuid.NewString()
		pgtest.MustExec(t, admin, `INSERT INTO projects (id, tenant_id, name, slug) VALUES ('`+other+`', '`+tenantA+`', 'Other', 'other')`)
		channel := func(project string) string {
			id := uuid.NewString()
			pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO notification_channels (id, tenant_id, type, config_encrypted, reminder_minutes, project_id)
				VALUES ('%s', '%s', 'slack_webhook', '\x01', 5, %s)`, id, tenantA, project))
			return id
		}
		inDefault, inOther, orgWide := channel("'"+defA+"'"), channel("'"+other+"'"), channel("NULL")
		// No selection: the channels of the monitor's own project only.
		fallback := seedMonitor(t, admin, tenantA, "now()", false, false)
		// Selection: the selected channels that are organization-wide or of its project; a stray
		// link to another project's channel (never created by the api) is ignored.
		selecting := seedMonitor(t, admin, tenantA, "now()", false, false)
		pgtest.MustExec(t, admin,
			`INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES ('`+tenantA+`', '`+selecting+`', '`+orgWide+`'), ('`+tenantA+`', '`+selecting+`', '`+inOther+`')`)
		// Only a stray link to another project's channel (only possible by direct SQL since the
		// contract step): not a valid selection, so the project fallback applies (T-13).
		stray := seedMonitor(t, admin, tenantA, "now()", false, false)
		pgtest.MustExec(t, admin,
			`INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES ('`+tenantA+`', '`+stray+`', '`+inOther+`')`)
		incidents := map[string]string{}
		for _, m := range []string{fallback, selecting, stray} {
			id := uuid.NewString()
			incidents[m] = id
			pgtest.MustExec(t, admin, `INSERT INTO incidents (id, tenant_id, monitor_id, started_at, reason) VALUES ('`+id+`', '`+tenantA+`', '`+m+`', now() - interval '1 hour', 'timeout')`)
		}
		if _, err := worker.Exec(ctx, `SELECT enqueue_incident_reminders(100)`); err != nil {
			t.Fatal(err)
		}
		rows, err := admin.Query(ctx, `SELECT monitor_id::text || '>' || channel_id::text FROM notification_deliveries WHERE kind = 'reminder' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		// chanA (Default project) has no reminders configured.
		want := map[string]bool{fallback + ">" + inDefault: true, selecting + ">" + orgWide: true, stray + ">" + inDefault: true}
		if len(got) != len(want) {
			t.Fatalf("reminders %v, want %v", got, want)
		}
		for _, g := range got {
			if !want[g] {
				t.Fatalf("unexpected reminder %s (all: %v)", g, got)
			}
		}
	})
}
