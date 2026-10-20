package migrations_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

func TestIntegrationMigrateFromScratchAndRerun(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("migrations"))
	ctx := context.Background()

	n, err := env.Migrate(ctx)
	if err != nil || n != 0 {
		t.Fatalf("second run applied %d migrations, err=%v; want 0", n, err)
	}
	admin := env.AdminConn(t)
	var tables []string
	// Partitions are named after the current month; they are checked in results_integration_test.go.
	rows, _ := admin.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
		WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition AND c.relname <> 'goose_db_version' ORDER BY 1`)
	tables, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := "audit_log,auth_identities,check_results,memberships,monitor_location_state,monitor_locations,monitor_state,monitors,probe_locations,result_rollups_hourly,sessions,tenants,users"
	if got := strings.Join(tables, ","); got != want {
		t.Fatalf("tables = %s, want %s", got, want)
	}
}

// definerAllowList names every SECURITY DEFINER function and the roles allowed to execute it.
// A new definer function fails the lint until it is reviewed and added here (ADR-0016).
var definerAllowList = map[string][]string{
	"auth_lookup_session":     {"maiak_api"},
	"ensure_partitions":       {"maiak_worker"},
	"claim_due_monitors":      {"maiak_worker"},
	"compute_rollups":         {"maiak_worker"},
	"drop_expired_partitions": {"maiak_worker"},
	"purge_expired_sessions":  {"maiak_worker"},
}

// extraPolicies are the permissive policies on tenant tables that are not tenant isolation
// policies, keyed by "table.policy", with the only command and roles they may have.
var extraPolicies = map[string]struct {
	cmd   string
	roles []string
}{
	// The SECURITY DEFINER session lookup runs as maiak_owner before the tenant is known (ADR-0004).
	"sessions.owner_session_lookup": {cmd: "r", roles: []string{"maiak_owner"}},
	// The cross-tenant definer functions of the results migration (ADR-0016, ADR-0018).
	"sessions.owner_session_purge":             {cmd: "d", roles: []string{"maiak_owner"}},
	"monitors.owner_claim_read":                {cmd: "r", roles: []string{"maiak_owner"}},
	"monitor_location_state.owner_claim_read":  {cmd: "r", roles: []string{"maiak_owner"}},
	"monitor_location_state.owner_claim_lease": {cmd: "w", roles: []string{"maiak_owner"}},
	"check_results.owner_rollup_read":          {cmd: "r", roles: []string{"maiak_owner"}},
	"result_rollups_hourly.owner_rollup_write": {cmd: "*", roles: []string{"maiak_owner"}},
}

var runtimeRoles = []string{"maiak_api", "maiak_worker"}

// TestIntegrationSchemaLint is the guard rail for new migrations: a tenant table without forced
// RLS and a tenant isolation policy, an extra permissive policy, a runtime role owning objects,
// bypassing RLS or being a member of maiak_owner, or a SECURITY DEFINER function that is not
// allow-listed, has no pinned search_path or is executable by others fails CI here.
func TestIntegrationSchemaLint(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("schemalint"))
	ctx := context.Background()
	admin := env.AdminConn(t)

	if v := schemaViolations(t, ctx, admin); len(v) > 0 {
		t.Fatalf("schema lint failed:\n  %s", strings.Join(v, "\n  "))
	}

	tenantTable := `CREATE TABLE lint_probe (id uuid PRIMARY KEY, tenant_id uuid NOT NULL);
		ALTER TABLE lint_probe ENABLE ROW LEVEL SECURITY;
		ALTER TABLE lint_probe FORCE ROW LEVEL SECURITY;`
	isolation := `CREATE POLICY tenant_isolation ON lint_probe
		USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
		WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);`
	definer := `CREATE FUNCTION lint_definer() RETURNS int LANGUAGE sql SECURITY DEFINER
		SET search_path = public, pg_temp AS 'SELECT 1';
		ALTER FUNCTION lint_definer() OWNER TO maiak_owner;
		REVOKE ALL ON FUNCTION lint_definer() FROM PUBLIC;`

	// Each rule must catch its violation. Every probe runs in a transaction that is rolled back.
	cases := []struct {
		name  string
		setup string
		want  []string
	}{
		{"forgotten RLS and policy, runtime owner",
			`CREATE TABLE lint_probe (id uuid PRIMARY KEY, tenant_id uuid NOT NULL);
			 ALTER TABLE lint_probe OWNER TO maiak_api;`,
			[]string{"lint_probe: row level security not enabled", "lint_probe: row level security not forced",
				"lint_probe: no policy", "maiak_api owns table lint_probe"}},
		{"USING (true) as the only policy",
			tenantTable + `CREATE POLICY open ON lint_probe USING (true) WITH CHECK (true);`,
			[]string{"lint_probe: no tenant isolation policy", "lint_probe: policy open is not a tenant isolation policy"}},
		{"isolation policy for SELECT only",
			tenantTable + `CREATE POLICY tenant_isolation ON lint_probe FOR SELECT
				USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);`,
			[]string{"lint_probe: no tenant isolation policy"}},
		{"isolation policy without WITH CHECK",
			tenantTable + `CREATE POLICY tenant_isolation ON lint_probe
				USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);`,
			[]string{"lint_probe: no tenant isolation policy"}},
		{"isolation policy for another role",
			tenantTable + `CREATE POLICY tenant_isolation ON lint_probe TO maiak_owner
				USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
				WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);`,
			[]string{"lint_probe: no tenant isolation policy"}},
		{"extra permissive policy next to a valid one",
			tenantTable + isolation + `CREATE POLICY peek ON lint_probe FOR SELECT TO maiak_api USING (true);`,
			[]string{"lint_probe: policy peek is not a tenant isolation policy"}},
		{"allow-listed policy with another shape",
			`ALTER POLICY owner_session_lookup ON sessions TO maiak_api;`,
			[]string{"sessions: policy owner_session_lookup must be FOR r TO maiak_owner"}},
		{"definer function not allow-listed",
			definer,
			[]string{"lint_definer: SECURITY DEFINER function not in the allow-list"}},
		{"definer function without pinned search_path",
			`ALTER FUNCTION auth_lookup_session(bytea) RESET search_path;`,
			[]string{"auth_lookup_session: SECURITY DEFINER function without pinned search_path"}},
		{"definer function not owned by maiak_owner",
			`ALTER FUNCTION auth_lookup_session(bytea) OWNER TO CURRENT_USER;`,
			[]string{"auth_lookup_session: SECURITY DEFINER function not owned by maiak_owner"}},
		{"definer function granted to another runtime role",
			`GRANT EXECUTE ON FUNCTION auth_lookup_session(bytea) TO maiak_worker;`,
			[]string{"auth_lookup_session: EXECUTE granted to maiak_worker", "auth_lookup_session: maiak_worker may execute it"}},
		{"definer function executable by PUBLIC",
			`GRANT EXECUTE ON FUNCTION auth_lookup_session(bytea) TO PUBLIC;`,
			[]string{"auth_lookup_session: executable by PUBLIC"}},
		{"partition without RLS",
			`CREATE TABLE lint_part PARTITION OF check_results FOR VALUES FROM ('2001-01-01 00:00:00+00') TO ('2001-02-01 00:00:00+00');`,
			[]string{"lint_part: row level security not enabled", "lint_part: row level security not forced", "lint_part: no policy"}},
		{"runtime role member of maiak_owner",
			`GRANT maiak_owner TO maiak_api;`,
			[]string{"maiak_api: member of maiak_owner"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, c.setup); err != nil {
				t.Fatalf("setup: %v", err)
			}
			v := strings.Join(schemaViolations(t, ctx, tx), "\n")
			for _, want := range c.want {
				if !strings.Contains(v, want) {
					t.Errorf("lint did not report %q; got:\n%s", want, v)
				}
			}
		})
	}
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func schemaViolations(t *testing.T, ctx context.Context, q querier) []string {
	t.Helper()
	var out []string
	collect := func(sql string, args ...any) {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			t.Fatalf("lint query: %v\n%s", err, sql)
		}
		v, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v...)
	}

	// Every table (including partitions) in public with a tenant_id column.
	collect(`
SELECT c.relname || ': ' || problem
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
CROSS JOIN LATERAL (VALUES
    (NOT c.relrowsecurity, 'row level security not enabled'),
    (NOT c.relforcerowsecurity, 'row level security not forced'),
    (NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid), 'no policy')
) AS checks(failed, problem)
WHERE c.relkind IN ('r', 'p') AND checks.failed
ORDER BY 1`)
	out = append(out, policyViolations(t, ctx, q)...)

	// Runtime roles: never superuser, never BYPASSRLS, never members of maiak_owner (they would
	// inherit ownership and FORCE RLS would not stop them), and they own nothing.
	collect(`
SELECT rolname || ': ' || problem
FROM pg_roles
CROSS JOIN LATERAL (VALUES
    (rolsuper, 'is superuser'),
    (rolbypassrls, 'has BYPASSRLS'),
    (pg_has_role(oid, 'maiak_owner', 'MEMBER'), 'member of maiak_owner')
) AS checks(failed, problem)
WHERE rolname = ANY($1) AND checks.failed`, runtimeRoles)
	collect(`
WITH r AS (SELECT oid, rolname FROM pg_roles WHERE rolname = ANY($1))
SELECT r.rolname || ' owns ' || kind || ' ' || name FROM (
    SELECT relowner AS owner, CASE relkind WHEN 'S' THEN 'sequence' WHEN 'v' THEN 'view' WHEN 'i' THEN 'index' ELSE 'table' END AS kind, relname::text AS name FROM pg_class
    UNION ALL SELECT proowner, 'function', proname::text FROM pg_proc
    UNION ALL SELECT nspowner, 'schema', nspname::text FROM pg_namespace
    UNION ALL SELECT typowner, 'type', typname::text FROM pg_type
    UNION ALL SELECT datdba, 'database', datname::text FROM pg_database
) o JOIN r ON r.oid = o.owner
ORDER BY 1`, runtimeRoles)

	out = append(out, definerViolations(t, ctx, q)...)
	return out
}

// policyViolations requires on every tenant table a permissive policy for all commands, to
// PUBLIC or runtime roles only, whose USING and WITH CHECK both read app.tenant_id, and rejects
// every other permissive policy unless it is in extraPolicies with exactly that shape.
// Permissive policies are OR-ed, so one stray USING (true) would expose all tenants.
func policyViolations(t *testing.T, ctx context.Context, q querier) []string {
	t.Helper()
	rows, err := q.Query(ctx, `
SELECT c.relname::text, p.polname::text, p.polcmd::text, p.polpermissive,
       ARRAY(SELECT CASE WHEN r = 0 THEN 'public' ELSE pg_get_userbyid(r)::text END FROM unnest(p.polroles) AS r ORDER BY 1),
       COALESCE(pg_get_expr(p.polqual, p.polrelid), ''), COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
JOIN pg_policy p ON p.polrelid = c.oid
WHERE c.relkind IN ('r', 'p')
ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("lint policies: %v", err)
	}
	type policy struct {
		table, name, cmd string
		permissive       bool
		roles            []string
		qual, check      string
	}
	pols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (policy, error) {
		var p policy
		err := r.Scan(&p.table, &p.name, &p.cmd, &p.permissive, &p.roles, &p.qual, &p.check)
		return p, err
	})
	if err != nil {
		t.Fatal(err)
	}
	allowedRoles := append([]string{"public"}, runtimeRoles...)
	isolated := map[string]bool{}
	var out []string
	for _, p := range pols {
		if !p.permissive {
			continue // restrictive policies only narrow access
		}
		rolesOK := len(p.roles) > 0
		for _, r := range p.roles {
			rolesOK = rolesOK && slices.Contains(allowedRoles, r)
		}
		if p.cmd == "*" && rolesOK && strings.Contains(p.qual, "app.tenant_id") && strings.Contains(p.check, "app.tenant_id") {
			isolated[p.table] = true
			continue
		}
		extra, ok := extraPolicies[p.table+"."+p.name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s: policy %s is not a tenant isolation policy and not allow-listed", p.table, p.name))
		case p.cmd != extra.cmd || !slices.Equal(p.roles, extra.roles):
			out = append(out, fmt.Sprintf("%s: policy %s must be FOR %s TO %s (is FOR %s TO %s)",
				p.table, p.name, extra.cmd, strings.Join(extra.roles, ","), p.cmd, strings.Join(p.roles, ",")))
		}
	}
	tables := map[string]bool{}
	for _, p := range pols {
		tables[p.table] = true
	}
	for tbl := range tables {
		if !isolated[tbl] {
			out = append(out, tbl+": no tenant isolation policy (permissive, all commands, PUBLIC or runtime roles, USING and WITH CHECK on app.tenant_id)")
		}
	}
	slices.Sort(out)
	return out
}

// definerViolations checks every SECURITY DEFINER function in public against definerAllowList.
func definerViolations(t *testing.T, ctx context.Context, q querier) []string {
	t.Helper()
	rows, err := q.Query(ctx, `
SELECT p.proname::text,
       pg_get_userbyid(p.proowner)::text,
       EXISTS (SELECT 1 FROM unnest(p.proconfig) AS c WHERE c LIKE 'search_path=%'),
       has_function_privilege('public', p.oid, 'EXECUTE'),
       ARRAY(SELECT DISTINCT pg_get_userbyid(a.grantee)::text
             FROM aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) AS a
             WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> 0 AND a.grantee <> p.proowner
             ORDER BY 1),
       ARRAY(SELECT r FROM unnest($1::text[]) AS r WHERE has_function_privilege(r, p.oid, 'EXECUTE') ORDER BY 1)
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'public'
WHERE p.prosecdef
ORDER BY 1`, runtimeRoles)
	if err != nil {
		t.Fatalf("lint definer functions: %v", err)
	}
	var out []string
	for rows.Next() {
		var name, owner string
		var pinned, public bool
		var grantees, executors []string
		if err := rows.Scan(&name, &owner, &pinned, &public, &grantees, &executors); err != nil {
			t.Fatal(err)
		}
		allowed, listed := definerAllowList[name]
		if !listed {
			out = append(out, name+": SECURITY DEFINER function not in the allow-list (definerAllowList)")
		}
		if owner != "maiak_owner" {
			out = append(out, name+": SECURITY DEFINER function not owned by maiak_owner")
		}
		if !pinned {
			out = append(out, name+": SECURITY DEFINER function without pinned search_path")
		}
		if public {
			out = append(out, name+": executable by PUBLIC")
		}
		for _, g := range grantees {
			if !slices.Contains(allowed, g) {
				out = append(out, name+": EXECUTE granted to "+g)
			}
		}
		// Effective privilege also catches a grant through a group role.
		for _, r := range executors {
			if !slices.Contains(allowed, r) {
				out = append(out, name+": "+r+" may execute it")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
