package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackPlanFeaturesLastOrg undoes the O-08 migration, so a test can replay an older migration
// (goose refuses to apply a missing older version).
var rollbackPlanFeaturesLastOrg = []string{
	`DELETE FROM plans WHERE key = 'community'`,
	`ALTER TABLE plans DROP COLUMN IF EXISTS features`,
	`CREATE OR REPLACE FUNCTION auth_login_tenant(p_user_id uuid) RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
	 SET search_path = public, pg_temp AS $$
	    SELECT m.tenant_id FROM memberships AS m JOIN tenants AS t ON t.id = m.tenant_id
	    WHERE m.user_id = p_user_id
	    ORDER BY (t.deleted_at IS NULL) DESC, (t.status = 'active') DESC, (t.slug = 'default') DESC, m.created_at, m.tenant_id
	    LIMIT 1
	$$`,
	`ALTER TABLE memberships DROP COLUMN IF EXISTS last_used_at`,
	`DELETE FROM goose_db_version WHERE version_id = 20261109150000`,
}

// TestIntegrationPlanFeaturesLastOrg checks the O-08 migration: plans carry features (empty for
// the seeded ones, no NULL names), the community plan has no quotas, and a login enters the last
// used organization before falling back to the old order (L-307).
func TestIntegrationPlanFeaturesLastOrg(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("planfeatures"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)

	var seeded string
	if err := admin.QueryRow(ctx, `SELECT string_agg(key || ':' || cardinality(features), ',' ORDER BY key) FROM plans`).Scan(&seeded); err != nil ||
		seeded != "community:0,free:0,internal:0" {
		t.Fatalf("plans = %q (%v)", seeded, err)
	}
	var unlimited bool
	if err := admin.QueryRow(ctx, `SELECT bool_and(v::bigint = 2147483647) AND count(*) = 6
		FROM plans, jsonb_each_text(limits) AS l(k, v) WHERE key = 'community'`).Scan(&unlimited); err != nil || !unlimited {
		t.Fatalf("community limits unlimited = %t (%v)", unlimited, err)
	}
	// Every limit the code knows is unlimited on community (plans.Resolve fails on a missing key).
	var raw []byte
	if err := admin.QueryRow(ctx, `SELECT limits FROM plans WHERE key = 'community'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	l, err := plans.Resolve(plans.KeyCommunity, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []plans.Resource{plans.ResourceMonitor, plans.ResourceHeartbeat, plans.ResourceChannel,
		plans.ResourceMember, plans.ResourceProject, plans.ResourceStatusPage} {
		if l.Of(r) != plans.Unlimited {
			t.Errorf("community %s limit = %d, want plans.Unlimited", r, l.Of(r))
		}
	}
	if _, err := api.Exec(ctx, `UPDATE plans SET features = '{x}'`); err == nil {
		t.Fatal("the api changed plan features")
	}
	// Feature names are slugs.
	for _, bad := range []string{`'{Bad}'`, `'{"a b"}'`, `'{""}'`, `ARRAY['` + strings.Repeat("a", 65) + `']`, `'{NULL}'`} {
		if _, err := admin.Exec(ctx, `UPDATE plans SET features = `+bad+` WHERE key = 'free'`); err == nil {
			t.Errorf("feature names %s accepted", bad)
		}
	}
	pgtest.MustExec(t, admin, `UPDATE plans SET features = '{sso,audit-export,a_b}' WHERE key = 'free'`,
		`UPDATE plans SET features = '{}' WHERE key = 'free'`)

	const older, newer = "01920000-0000-7000-8000-0000000000f1", "01920000-0000-7000-8000-0000000000f2"
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+older+`', 'older', 'Older'), ('`+newer+`', 'newer', 'Newer')`,
		`INSERT INTO users (id, email) VALUES ('`+userA+`', 'a@example.com')`,
		`INSERT INTO memberships (tenant_id, user_id, role, created_at) VALUES
			('`+older+`', '`+userA+`', 'owner', now() - interval '2 days'), ('`+newer+`', '`+userA+`', 'member', now() - interval '1 day')`)
	loginTenant := func() string {
		t.Helper()
		var id string
		if err := api.QueryRow(ctx, `SELECT auth_login_tenant($1)::text`, userA).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if got := loginTenant(); got != older {
		t.Fatalf("never used: login enters %s, want the oldest membership %s", got, older)
	}
	pgtest.MustExec(t, admin, `UPDATE memberships SET last_used_at = now() WHERE tenant_id = '`+newer+`'`)
	if got := loginTenant(); got != newer {
		t.Fatalf("login enters %s, want the last used %s", got, newer)
	}
	pgtest.MustExec(t, admin, `UPDATE memberships SET last_used_at = now() + interval '1 minute' WHERE tenant_id = '`+older+`'`)
	if got := loginTenant(); got != older {
		t.Fatalf("login enters %s, want the last used %s", got, older)
	}
	// A suspended organization still loses against an active one, even when used last.
	pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id = '`+older+`'`)
	if got := loginTenant(); got != newer {
		t.Fatalf("login enters %s, want the active %s", got, newer)
	}
}
