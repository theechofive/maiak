package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationCreateTenantPlan checks auth_create_tenant (P-07, ADR-0095): the plan is chosen
// by the function (internal only for the default slug, free otherwise), a taken slug returns
// NULL without aborting the transaction, and the slug check still applies.
func TestIntegrationCreateTenantPlan(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("createtenant"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)

	create := func(slug string) (*uuid.UUID, error) {
		var id *uuid.UUID
		err := api.QueryRow(ctx, `SELECT auth_create_tenant($1, $2, 'Name')`, uuid.New(), slug).Scan(&id)
		return id, err
	}
	for _, slug := range []string{"default", "t-abcdefghij", "internal"} {
		if id, err := create(slug); err != nil || id == nil {
			t.Fatalf("create %s: id=%v err=%v", slug, id, err)
		}
	}
	if id, err := create("t-abcdefghij"); err != nil || id != nil {
		t.Fatalf("taken slug: id=%v err=%v, want NULL", id, err)
	}
	if _, err := create("Not A Slug"); err == nil || !strings.Contains(err.Error(), "tenants_slug_check") {
		t.Fatalf("invalid slug: err = %v", err)
	}
	var plans string
	if err := admin.QueryRow(ctx, `SELECT string_agg(slug || '=' || plan_key, ',' ORDER BY slug) FROM tenants`).Scan(&plans); err != nil {
		t.Fatal(err)
	}
	if plans != "default=internal,internal=free,t-abcdefghij=free" {
		t.Fatalf("plans = %s", plans)
	}

	// tenants.limits must be a JSON object.
	for _, v := range []string{`[]`, `"x"`, `null`, `1`} {
		if _, err := admin.Exec(ctx, `UPDATE tenants SET limits = $1::jsonb WHERE slug = 'default'`, v); err == nil ||
			!strings.Contains(err.Error(), "tenants_limits_is_object") {
			t.Errorf("limits %s: err = %v, want the check constraint", v, err)
		}
	}
	pgtest.MustExec(t, admin, `UPDATE tenants SET limits = '{"max_monitors": 5}' WHERE slug = 'default'`)
}

// TestIntegrationP07DefinersRefuseOtherRoles: no role but maiak_api (and the owner) may execute
// the P-07 definer functions; maiak_worker is covered by TestIntegrationDefinerCallsByRole.
func TestIntegrationP07DefinersRefuseOtherRoles(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("p07roles"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	for _, name := range []string{"auth_create_tenant", "auth_count_other_memberships", "auth_revoke_user_sessions"} {
		t.Run(name, func(t *testing.T) {
			tx, err := admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `CREATE ROLE p07_stranger NOLOGIN; GRANT USAGE ON SCHEMA public TO p07_stranger; SET LOCAL ROLE p07_stranger`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, definerCalls[name])
			if err == nil || !strings.Contains(err.Error(), "permission denied for function "+name) {
				t.Fatalf("stranger role: err = %v, want permission denied", err)
			}
		})
	}
}

// TestIntegrationAdoptionDefiners checks the BootstrapAdmin helpers: the membership count
// outside one tenant, and the revocation of a user's sessions in every tenant that leaves the
// caller's app.tenant_id and other users' sessions alone.
func TestIntegrationAdoptionDefiners(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("adoption"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	const (
		userOne   = "01920000-0000-7000-8000-0000000000d1"
		userTwo   = "01920000-0000-7000-8000-0000000000d2"
		tenantC   = "01920000-0000-7000-8000-00000000000c"
		tenantDel = "01920000-0000-7000-8000-00000000000d"
	)
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B'), ('`+tenantC+`', 'c', 'C')`,
		`INSERT INTO tenants (id, slug, name, deleted_at) VALUES ('`+tenantDel+`', 'gone', 'Gone', now())`,
		`INSERT INTO users (id, email) VALUES ('`+userOne+`', 'one@example.com'), ('`+userTwo+`', 'two@example.com')`,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES
			('`+tenantA+`', '`+userOne+`', 'owner'), ('`+tenantB+`', '`+userOne+`', 'viewer'),
			('`+tenantDel+`', '`+userOne+`', 'owner'), ('`+tenantA+`', '`+userTwo+`', 'owner')`,
		`INSERT INTO sessions (id, user_id, tenant_id, token_hash, csrf_token, expires_at)
		 SELECT gen_random_uuid(), u, t, sha256(gen_random_uuid()::text::bytea), 'x', now() + interval '1 hour'
		 FROM (VALUES ('`+userOne+`'::uuid, '`+tenantA+`'::uuid), ('`+userOne+`', '`+tenantA+`'), ('`+userOne+`', '`+tenantB+`'),
		              ('`+userTwo+`', '`+tenantA+`')) AS v(u, t)`)
	api := env.Conn(t, env.APIURL)

	count := func(user, tenant string) int {
		t.Helper()
		var n int
		if err := api.QueryRow(ctx, `SELECT auth_count_other_memberships($1, $2)`, user, tenant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Deleted tenants count too: a membership there still means another owner's user.
	if got := count(userOne, tenantA); got != 2 {
		t.Fatalf("one outside A = %d, want 2", got)
	}
	if got := count(userOne, tenantC); got != 3 {
		t.Fatalf("one outside C = %d, want 3", got)
	}
	if got := count(userTwo, tenantA); got != 0 {
		t.Fatalf("two outside A = %d, want 0", got)
	}

	for _, caller := range []string{tenantC, ""} {
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", caller); err != nil {
			t.Fatal(err)
		}
		var n int
		var after string
		if err := tx.QueryRow(ctx, `SELECT auth_revoke_user_sessions($1)`, userOne).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		want := 3
		if caller == "" {
			want = 0 // already revoked
		}
		if n != want || after != caller {
			t.Fatalf("caller %q: revoked %d (want %d), app.tenant_id after = %q", caller, n, want, after)
		}
	}
	rows, err := admin.Query(ctx, `SELECT user_id::text FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != userTwo {
		t.Fatalf("sessions left = %v, want only %s", left, userTwo)
	}
}
