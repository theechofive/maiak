package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackPlatformAdmin undoes the platform admin migration (P-04), so a test that replays an
// older migration can roll back the later ones too (goose refuses a missing older version). IF
// EXISTS: a failed replay leaves the migration unapplied, and the next replay rolls back again.
var rollbackPlatformAdmin = []string{
	`DROP FUNCTION IF EXISTS admin_stats(uuid)`,
	`DROP FUNCTION IF EXISTS admin_reset_user_password(uuid, uuid, text, uuid, inet)`,
	`DROP FUNCTION IF EXISTS admin_list_users(uuid, uuid, text, integer, integer)`,
	`DROP FUNCTION IF EXISTS admin_delete_tenant(uuid, uuid, uuid, inet)`,
	`DROP FUNCTION IF EXISTS admin_set_tenant_status(uuid, uuid, text, uuid, inet)`,
	`DROP FUNCTION IF EXISTS admin_set_tenant_plan(uuid, uuid, text, uuid, inet)`,
	`DROP FUNCTION IF EXISTS admin_get_tenant(uuid, uuid)`,
	`DROP FUNCTION IF EXISTS admin_list_tenants(uuid, text, boolean, integer, integer)`,
	`DROP FUNCTION IF EXISTS auth_sync_platform_admin(uuid)`,
	`DROP FUNCTION IF EXISTS admin_audit(uuid, uuid, uuid, text, text, text, jsonb, inet)`,
	`DROP FUNCTION IF EXISTS admin_require_platform_admin(uuid)`,
	`GRANT INSERT ON users TO maiak_api`,
	`ALTER TABLE users DROP COLUMN IF EXISTS is_platform_admin`,
	`DELETE FROM goose_db_version WHERE version_id = 20261021190000`,
}

// adminDefiners are the definer functions of the platform admin migration.
var adminDefiners = []string{
	"auth_sync_platform_admin", "admin_list_tenants", "admin_get_tenant", "admin_set_tenant_plan",
	"admin_set_tenant_status", "admin_delete_tenant", "admin_list_users", "admin_reset_user_password", "admin_stats",
}

const (
	userPlatform = "01920000-0000-7000-8000-0000000000e1"
	userOwnerA   = "01920000-0000-7000-8000-0000000000e2"
	userOwnerB   = "01920000-0000-7000-8000-0000000000e3"
	tenantDef    = "01920000-0000-7000-8000-0000000000ee"
	// A valid-looking argon2id PHC string; the functions store it without verifying it.
	fakeHash = `$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g`
)

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// seedPlatform creates the default tenant with the platform admin (flag set directly), tenant A
// (owner userOwnerA, two http monitors, one heartbeat, one deleted monitor, one session) and
// tenant B (owner userOwnerB).
func seedPlatform(t *testing.T, admin *pgx.Conn) {
	t.Helper()
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name, plan_key, created_at) VALUES
			('`+tenantDef+`', 'default', 'Default', 'internal', now() - interval '3 days'),
			('`+tenantA+`', 't-aaaa', 'Alpha Corp', 'free', now() - interval '2 days'),
			('`+tenantB+`', 't-bbbb', 'Beta Ltd', 'free', now() - interval '1 day')`,
		`INSERT INTO users (id, email, is_platform_admin) VALUES
			('`+userPlatform+`', 'admin', true), ('`+userOwnerA+`', 'alice@alpha.example', false), ('`+userOwnerB+`', 'bob@beta.example', false)`,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES
			('`+tenantDef+`', '`+userPlatform+`', 'owner'), ('`+tenantA+`', '`+userOwnerA+`', 'owner'), ('`+tenantB+`', '`+userOwnerB+`', 'owner')`,
		`INSERT INTO sessions (id, user_id, tenant_id, token_hash, csrf_token, expires_at)
		 VALUES (gen_random_uuid(), '`+userOwnerA+`', '`+tenantA+`', sha256('a-session'::bytea), 'x', now() + interval '1 hour')`)
	seedMonitor(t, admin, tenantA, "now()", false, false)
	seedMonitor(t, admin, tenantA, "now()", true, false)
	seedMonitor(t, admin, tenantA, "now()", false, true)
	seedHeartbeat(t, admin, tenantA, "token-a", 300, 60, "")
}

// TestIntegrationPlatformAdminGrants checks the flag's protection (P-04, ADR-0097): the api role
// cannot set it directly, only auth_sync_platform_admin does, and only for an owner of the default
// tenant without other memberships; no other role may execute the admin functions.
func TestIntegrationPlatformAdminGrants(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("platformadmin_grants"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	seedPlatform(t, admin)
	pgtest.MustExec(t, admin, `UPDATE users SET is_platform_admin = false`)

	for _, stmt := range []string{
		`INSERT INTO users (id, email, is_platform_admin) VALUES (gen_random_uuid(), 'x@example.com', true)`,
		`INSERT INTO users (id, email, status) VALUES (gen_random_uuid(), 'y@example.com', 'active')`,
		`UPDATE users SET is_platform_admin = true WHERE id = '` + userOwnerA + `'`,
	} {
		if _, err := api.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: err = %v, want permission denied", stmt, err)
		}
	}
	// Registration and the bootstrap still insert users (the previous release too).
	if _, err := api.Exec(ctx, `INSERT INTO users (id, email, password_hash, display_name) VALUES (gen_random_uuid(), 'z@example.com', NULL, '')
		ON CONFLICT (email) DO NOTHING`); err != nil {
		t.Fatalf("api inserts a user: %v", err)
	}
	var flagged bool
	if err := admin.QueryRow(ctx, `SELECT is_platform_admin FROM users WHERE email = 'z@example.com'`).Scan(&flagged); err != nil || flagged {
		t.Fatalf("new user flag = %v (err %v), want false", flagged, err)
	}

	sync := func(user string) (int, error) {
		var n int
		err := api.QueryRow(ctx, `SELECT auth_sync_platform_admin($1)`, user).Scan(&n)
		return n, err
	}
	// A registered owner of another tenant can never obtain the flag, not even through the function.
	if _, err := sync(userOwnerA); sqlState(err) != "MKA01" {
		t.Fatalf("sync of a registered owner: err = %v, want MKA01", err)
	}
	if n, err := sync(userPlatform); err != nil || n != 1 {
		t.Fatalf("sync of the default owner: n=%d err=%v", n, err)
	}
	if n, err := sync(userPlatform); err != nil || n != 0 {
		t.Fatalf("sync again: n=%d err=%v, want 0 (idempotent)", n, err)
	}
	// A default owner that is also a member elsewhere is refused (ADR-0095).
	pgtest.MustExec(t, admin, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('`+tenantB+`', '`+userPlatform+`', 'viewer')`)
	if _, err := sync(userPlatform); sqlState(err) != "MKA01" {
		t.Fatalf("sync of a default owner with another membership: err = %v, want MKA01", err)
	}
	pgtest.MustExec(t, admin, `DELETE FROM memberships WHERE tenant_id = '`+tenantB+`' AND user_id = '`+userPlatform+`'`)
	// A new configured admin takes the flag from the retired one.
	const next = "01920000-0000-7000-8000-0000000000e9"
	pgtest.MustExec(t, admin, `INSERT INTO users (id, email) VALUES ('`+next+`', 'root')`,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ('`+tenantDef+`', '`+next+`', 'owner')`)
	if n, err := sync(next); err != nil || n != 2 {
		t.Fatalf("sync of the next admin: n=%d err=%v, want 2", n, err)
	}
	var admins string
	if err := admin.QueryRow(ctx, `SELECT string_agg(email::text, ',' ORDER BY email) FROM users WHERE is_platform_admin`).Scan(&admins); err != nil || admins != "root" {
		t.Fatalf("platform admins = %q (err %v), want root", admins, err)
	}

	for _, name := range adminDefiners {
		t.Run("stranger/"+name, func(t *testing.T) {
			tx, err := admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `CREATE ROLE p04_stranger NOLOGIN; GRANT USAGE ON SCHEMA public TO p04_stranger; SET LOCAL ROLE p04_stranger`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, definerCalls[name])
			if err == nil || !strings.Contains(err.Error(), "permission denied for function "+name) {
				t.Fatalf("stranger role: err = %v, want permission denied", err)
			}
		})
	}
	// The internal helpers are not callable by the runtime roles at all.
	for _, stmt := range []string{
		`SELECT admin_require_platform_admin('` + userPlatform + `')`,
		`SELECT admin_audit('` + tenantA + `', NULL, gen_random_uuid(), 'x', 'x', NULL, '{}', NULL)`,
	} {
		for _, url := range []string{env.APIURL, env.WorkerURL} {
			if _, err := env.Conn(t, url).Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s: err = %v, want permission denied", stmt, err)
			}
		}
	}
}

// TestIntegrationAdminFunctions exercises every admin function as maiak_api: the in-function
// check of the actor, the happy paths, the guard rails and the audit rows.
func TestIntegrationAdminFunctions(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("platformadmin_funcs"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	seedPlatform(t, admin)

	exec := func(stmt string, args ...any) error {
		_, err := api.Exec(ctx, stmt, args...)
		return err
	}
	wantState := func(err error, code string) {
		t.Helper()
		if sqlState(err) != code {
			t.Fatalf("err = %v, want SQLSTATE %s", err, code)
		}
	}

	t.Run("a non-admin actor is refused by every function", func(t *testing.T) {
		calls := []string{
			`SELECT * FROM admin_list_tenants($1, '', true, 25, 0)`,
			`SELECT * FROM admin_get_tenant($1, '` + tenantA + `')`,
			`SELECT admin_set_tenant_plan($1, '` + tenantA + `', 'internal', gen_random_uuid(), NULL)`,
			`SELECT admin_set_tenant_status($1, '` + tenantB + `', 'suspended', gen_random_uuid(), NULL)`,
			`SELECT admin_delete_tenant($1, '` + tenantB + `', gen_random_uuid(), NULL)`,
			`SELECT * FROM admin_list_users($1, '` + tenantA + `', '', 25, 0)`,
			`SELECT * FROM admin_reset_user_password($1, '` + userOwnerB + `', '` + fakeHash + `', gen_random_uuid(), NULL)`,
			`SELECT admin_stats($1)`,
		}
		for _, actor := range []any{userOwnerA, uuid.NewString(), nil} {
			for _, c := range calls {
				if err := exec(c, actor); sqlState(err) != "MKA01" {
					t.Errorf("%s as %v: err = %v, want MKA01", c, actor, err)
				}
			}
		}
		// A disabled platform admin is refused too.
		pgtest.MustExec(t, admin, `UPDATE users SET status = 'disabled' WHERE id = '`+userPlatform+`'`)
		wantState(exec(`SELECT admin_stats($1)`, userPlatform), "MKA01")
		pgtest.MustExec(t, admin, `UPDATE users SET status = 'active' WHERE id = '`+userPlatform+`'`)
		var changed string
		if err := admin.QueryRow(ctx, `SELECT plan_key || status FROM tenants WHERE id = $1`, tenantA).Scan(&changed); err != nil || changed != "freeactive" {
			t.Fatalf("tenant A changed by refused calls: %s %v", changed, err)
		}
	})

	listTenants := func(query string, includeDeleted bool, limit, offset int) ([]string, int) {
		t.Helper()
		rows, err := api.Query(ctx, `SELECT slug, total_count FROM admin_list_tenants($1, $2, $3, $4, $5)`, userPlatform, query, includeDeleted, limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		var slugs []string
		total := 0
		for rows.Next() {
			var s string
			if err := rows.Scan(&s, &total); err != nil {
				t.Fatal(err)
			}
			slugs = append(slugs, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return slugs, total
	}

	t.Run("list, search and pagination", func(t *testing.T) {
		if s, n := listTenants("", false, 25, 0); fmt.Sprint(s) != "[t-bbbb t-aaaa default]" || n != 3 {
			t.Fatalf("all: %v %d", s, n)
		}
		if s, n := listTenants("", false, 1, 1); fmt.Sprint(s) != "[t-aaaa]" || n != 3 {
			t.Fatalf("page 2 of 1: %v %d", s, n)
		}
		if s, n := listTenants("", false, 1000, 0); len(s) != 3 || n != 3 {
			t.Fatalf("limit is capped, not refused: %v %d", s, n)
		}
		for q, want := range map[string]string{
			"ALPHA":        "[t-aaaa]", // name, case-insensitive
			"t-bb":         "[t-bbbb]", // slug
			"bob@beta":     "[t-bbbb]", // a member's login
			tenantA:        "[t-aaaa]", // id
			"%":            "[]",       // no LIKE wildcards
			"nothing-here": "[]",
		} {
			if s, _ := listTenants(q, false, 25, 0); fmt.Sprint(s) != want {
				t.Errorf("query %q: %v, want %s", q, s, want)
			}
		}
		var users, monitors, heartbeats int
		var plan, planName string
		if err := api.QueryRow(ctx, `SELECT user_count, monitor_count, heartbeat_count, plan_key, plan_name FROM admin_get_tenant($1, $2)`,
			userPlatform, tenantA).Scan(&users, &monitors, &heartbeats, &plan, &planName); err != nil {
			t.Fatal(err)
		}
		if users != 1 || monitors != 2 || heartbeats != 1 || plan != "free" || planName != "Free" {
			t.Fatalf("tenant A: users=%d monitors=%d heartbeats=%d plan=%s/%s", users, monitors, heartbeats, plan, planName)
		}
		wantState(exec(`SELECT * FROM admin_get_tenant($1, gen_random_uuid())`, userPlatform), "MKA02")
	})

	auditRows := func(tenant, action string) []string {
		t.Helper()
		rows, err := admin.Query(ctx, `SELECT actor_user_id::text || ' ' || entity_id || ' ' || metadata::text FROM audit_log
			WHERE tenant_id = $1 AND action = $2 ORDER BY id`, tenant, action)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	changed := func(stmt string, args ...any) bool {
		t.Helper()
		var c bool
		if err := api.QueryRow(ctx, stmt, args...).Scan(&c); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		return c
	}

	t.Run("plan changes", func(t *testing.T) {
		wantState(exec(`SELECT admin_set_tenant_plan($1, $2, 'gold', gen_random_uuid(), NULL)`, userPlatform, tenantA), "MKA03")
		wantState(exec(`SELECT admin_set_tenant_plan($1, gen_random_uuid(), 'free', gen_random_uuid(), NULL)`, userPlatform), "MKA02")
		if changed(`SELECT admin_set_tenant_plan($1, $2, 'free', gen_random_uuid(), '203.0.113.9')`, userPlatform, tenantA) {
			t.Fatal("same plan reported as a change")
		}
		if !changed(`SELECT admin_set_tenant_plan($1, $2, 'internal', gen_random_uuid(), '203.0.113.9')`, userPlatform, tenantA) {
			t.Fatal("plan change not reported")
		}
		rows := auditRows(tenantA, "admin.tenant.plan_changed")
		if len(rows) != 1 || rows[0] != userPlatform+" "+tenantA+` {"to": "internal", "from": "free"}` {
			t.Fatalf("plan audit rows: %q", rows)
		}
		var ip string
		if err := admin.QueryRow(ctx, `SELECT host(ip) FROM audit_log WHERE action = 'admin.tenant.plan_changed'`).Scan(&ip); err != nil || ip != "203.0.113.9" {
			t.Fatalf("audit ip = %q (err %v)", ip, err)
		}
		// The plan change of the default tenant is allowed (only suspension and deletion are guarded).
		if !changed(`SELECT admin_set_tenant_plan($1, $2, 'free', gen_random_uuid(), NULL)`, userPlatform, tenantDef) {
			t.Fatal("default tenant plan change")
		}
		pgtest.MustExec(t, admin, `UPDATE tenants SET plan_key = 'internal' WHERE id = '`+tenantDef+`'`)
	})

	t.Run("suspension and its guard rails", func(t *testing.T) {
		wantState(exec(`SELECT admin_set_tenant_status($1, $2, 'paused', gen_random_uuid(), NULL)`, userPlatform, tenantA), "MKA04")
		wantState(exec(`SELECT admin_set_tenant_status($1, $2, 'suspended', gen_random_uuid(), NULL)`, userPlatform, tenantDef), "MKA05")
		// The admin may not suspend a tenant it belongs to.
		pgtest.MustExec(t, admin, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('`+tenantB+`', '`+userPlatform+`', 'viewer')`)
		wantState(exec(`SELECT admin_set_tenant_status($1, $2, 'suspended', gen_random_uuid(), NULL)`, userPlatform, tenantB), "MKA06")
		wantState(exec(`SELECT admin_delete_tenant($1, $2, gen_random_uuid(), NULL)`, userPlatform, tenantB), "MKA06")
		pgtest.MustExec(t, admin, `DELETE FROM memberships WHERE tenant_id = '`+tenantB+`' AND user_id = '`+userPlatform+`'`)
		// Unsuspending the default tenant is harmless and allowed (nothing changes).
		if changed(`SELECT admin_set_tenant_status($1, $2, 'active', gen_random_uuid(), NULL)`, userPlatform, tenantDef) {
			t.Fatal("default tenant was not active")
		}

		// The caller's app.tenant_id survives the audit insert into another tenant.
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB); err != nil {
			t.Fatal(err)
		}
		var ok bool
		var after string
		if err := tx.QueryRow(ctx, `SELECT admin_set_tenant_status($1, $2, 'suspended', gen_random_uuid(), NULL), current_setting('app.tenant_id')`,
			userPlatform, tenantA).Scan(&ok, &after); err != nil || !ok {
			t.Fatalf("suspend: %v %v", ok, err)
		}
		if err := tx.QueryRow(ctx, `SELECT current_setting('app.tenant_id')`).Scan(&after); err != nil || after != tenantB {
			t.Fatalf("app.tenant_id after the call = %q (err %v), want %s", after, err, tenantB)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if changed(`SELECT admin_set_tenant_status($1, $2, 'suspended', gen_random_uuid(), NULL)`, userPlatform, tenantA) {
			t.Fatal("second suspension reported as a change")
		}
		if rows := auditRows(tenantA, "admin.tenant.suspended"); len(rows) != 1 || !strings.HasSuffix(rows[0], `{"to": "suspended", "from": "active"}`) {
			t.Fatalf("suspension audit rows: %q", rows)
		}
		// Suspended tenants still list and keep their data.
		if s, _ := listTenants("alpha", false, 25, 0); fmt.Sprint(s) != "[t-aaaa]" {
			t.Fatalf("suspended tenant listed: %v", s)
		}
		if !changed(`SELECT admin_set_tenant_status($1, $2, 'active', gen_random_uuid(), NULL)`, userPlatform, tenantA) {
			t.Fatal("unsuspend not reported")
		}
		if rows := auditRows(tenantA, "admin.tenant.unsuspended"); len(rows) != 1 {
			t.Fatalf("unsuspend audit rows: %q", rows)
		}
	})

	t.Run("users and password reset", func(t *testing.T) {
		var login, role string
		var total int
		var isAdmin bool
		if err := api.QueryRow(ctx, `SELECT login, role, is_platform_admin, total_count FROM admin_list_users($1, $2, 'ALICE', 25, 0)`,
			userPlatform, tenantA).Scan(&login, &role, &isAdmin, &total); err != nil {
			t.Fatal(err)
		}
		if login != "alice@alpha.example" || role != "owner" || isAdmin || total != 1 {
			t.Fatalf("user row: %s %s %v %d", login, role, isAdmin, total)
		}
		if err := api.QueryRow(ctx, `SELECT is_platform_admin FROM admin_list_users($1, $2, '', 25, 0)`, userPlatform, tenantDef).Scan(&isAdmin); err != nil || !isAdmin {
			t.Fatalf("default tenant lists the platform admin: %v %v", isAdmin, err)
		}
		wantState(exec(`SELECT * FROM admin_list_users($1, gen_random_uuid(), '', 25, 0)`, userPlatform), "MKA02")

		reset := func(user, hash string) (string, int, error) {
			var tenant string
			var n int
			err := api.QueryRow(ctx, `SELECT tenant_id::text, sessions_revoked FROM admin_reset_user_password($1, $2, $3, gen_random_uuid(), NULL)`,
				userPlatform, user, hash).Scan(&tenant, &n)
			return tenant, n, err
		}
		if _, _, err := reset(userPlatform, fakeHash); sqlState(err) != "MKA07" {
			t.Fatalf("reset of the platform admin: err = %v, want MKA07", err)
		}
		if _, _, err := reset(userOwnerA, "plain text"); sqlState(err) != "MKA04" {
			t.Fatalf("reset with a non-argon2id value: err = %v, want MKA04", err)
		}
		if _, _, err := reset(uuid.NewString(), fakeHash); sqlState(err) != "MKA02" {
			t.Fatalf("reset of an unknown user: err = %v, want MKA02", err)
		}
		tenant, n, err := reset(userOwnerA, fakeHash)
		if err != nil || tenant != tenantA || n != 1 {
			t.Fatalf("reset: tenant=%s sessions=%d err=%v", tenant, n, err)
		}
		var stored string
		var sessions int
		if err := admin.QueryRow(ctx, `SELECT password_hash, (SELECT count(*) FROM sessions WHERE user_id = $1) FROM users WHERE id = $1`, userOwnerA).
			Scan(&stored, &sessions); err != nil || stored != fakeHash || sessions != 0 {
			t.Fatalf("after reset: hash stored=%v sessions=%d err=%v", stored == fakeHash, sessions, err)
		}
		rows := auditRows(tenantA, "admin.user.password_reset")
		if len(rows) != 1 || rows[0] != userPlatform+" "+userOwnerA+` {"sessions_revoked": 1}` {
			t.Fatalf("reset audit rows: %q", rows)
		}
	})

	t.Run("soft delete", func(t *testing.T) {
		wantState(exec(`SELECT admin_delete_tenant($1, $2, gen_random_uuid(), NULL)`, userPlatform, tenantDef), "MKA05")
		wantState(exec(`SELECT admin_delete_tenant($1, gen_random_uuid(), gen_random_uuid(), NULL)`, userPlatform), "MKA02")
		if !changed(`SELECT admin_delete_tenant($1, $2, gen_random_uuid(), NULL)`, userPlatform, tenantB) {
			t.Fatal("delete not reported")
		}
		if changed(`SELECT admin_delete_tenant($1, $2, gen_random_uuid(), NULL)`, userPlatform, tenantB) {
			t.Fatal("second delete reported as a change")
		}
		var status string
		var deleted bool
		if err := admin.QueryRow(ctx, `SELECT status, deleted_at IS NOT NULL FROM tenants WHERE id = $1`, tenantB).Scan(&status, &deleted); err != nil ||
			status != "suspended" || !deleted {
			t.Fatalf("deleted tenant: status=%s deleted=%v err=%v", status, deleted, err)
		}
		if rows := auditRows(tenantB, "admin.tenant.deleted"); len(rows) != 1 || !strings.HasSuffix(rows[0], `{"plan": "free", "status": "active"}`) {
			t.Fatalf("delete audit rows: %q", rows)
		}
		wantState(exec(`SELECT admin_set_tenant_plan($1, $2, 'internal', gen_random_uuid(), NULL)`, userPlatform, tenantB), "MKA08")
		wantState(exec(`SELECT admin_set_tenant_status($1, $2, 'active', gen_random_uuid(), NULL)`, userPlatform, tenantB), "MKA08")
		if s, n := listTenants("", false, 25, 0); fmt.Sprint(s) != "[t-aaaa default]" || n != 2 {
			t.Fatalf("list without deleted: %v %d", s, n)
		}
		if s, n := listTenants("", true, 25, 0); fmt.Sprint(s) != "[t-bbbb t-aaaa default]" || n != 3 {
			t.Fatalf("list with deleted: %v %d", s, n)
		}
		if err := exec(`SELECT * FROM admin_get_tenant($1, $2)`, userPlatform, tenantB); err != nil {
			t.Fatalf("a deleted tenant can still be read: %v", err)
		}
	})

	t.Run("stats", func(t *testing.T) {
		var raw string
		if err := api.QueryRow(ctx, `SELECT admin_stats($1)::text`, userPlatform).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		want := `{"plans": [{"key": "free", "name": "Free", "tenants": 0}, {"key": "internal", "name": "Internal", "tenants": 2}], ` +
			`"users": 3, "tenants": {"total": 2, "active": 2, "deleted": 1, "suspended": 0}, "monitors": 2, "heartbeats": 1}`
		if raw != want {
			t.Fatalf("stats = %s\nwant    %s", raw, want)
		}
	})

	t.Run("no secret in any admin audit row", func(t *testing.T) {
		var leaks int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action LIKE 'admin.%' AND metadata::text LIKE '%argon2%'`).Scan(&leaks); err != nil || leaks != 0 {
			t.Fatalf("audit rows with a hash: %d (err %v)", leaks, err)
		}
	})
}

// TestIntegrationSuspendedTenantsAreSkipped: suspended and deleted tenants get no Slack deliveries
// claimed (claim_deliveries, P-04) and no monitor claims (claim_due_monitors, B-20), and a user
// whose only tenant is deleted still resolves to it in auth_login_tenant (so the login can answer
// "suspended" after a correct password).
func TestIntegrationSuspendedTenantsAreSkipped(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("platformadmin_skip"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	worker := env.Conn(t, env.WorkerURL)
	api := env.Conn(t, env.APIURL)
	seedPlatform(t, admin)
	channel := uuid.NewString()
	monitorB := seedMonitor(t, admin, tenantB, "now() - interval '1 minute'", false, false)
	pgtest.MustExec(t, admin,
		`INSERT INTO notification_channels (id, tenant_id, type, config_encrypted) VALUES ('`+channel+`', '`+tenantB+`', 'slack_webhook', '\x01')`,
		`INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, channel_id, payload)
		 VALUES (gen_random_uuid(), '`+tenantB+`', 'tls_expiring', '`+monitorB+`', '`+channel+`', '{}')`)

	claimDeliveries := func() int {
		t.Helper()
		tx, err := worker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM claim_deliveries(10)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	claimMonitors := func() int {
		t.Helper()
		tx, err := worker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM claim_due_monitors(100, 'home-eu-central-1') WHERE tenant_id = $1`, tenantB).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if d, m := claimDeliveries(), claimMonitors(); d != 1 || m != 1 {
		t.Fatalf("active tenant: deliveries=%d monitors=%d, want 1 and 1", d, m)
	}
	for _, stmt := range []string{
		`UPDATE tenants SET status = 'suspended' WHERE id = '` + tenantB + `'`,
		`UPDATE tenants SET status = 'active', deleted_at = now() WHERE id = '` + tenantB + `'`,
	} {
		pgtest.MustExec(t, admin, stmt)
		if d, m := claimDeliveries(), claimMonitors(); d != 0 || m != 0 {
			t.Fatalf("after %s: deliveries=%d monitors=%d, want none", stmt, d, m)
		}
	}

	var got *uuid.UUID
	if err := api.QueryRow(ctx, `SELECT auth_login_tenant($1)`, userOwnerB).Scan(&got); err != nil || got == nil || got.String() != tenantB {
		t.Fatalf("login tenant of a user whose only tenant is deleted: %v (err %v), want %s", got, err, tenantB)
	}
	// A live membership still wins over a deleted one.
	pgtest.MustExec(t, admin, `INSERT INTO memberships (tenant_id, user_id, role, created_at) VALUES ('`+tenantA+`', '`+userOwnerB+`', 'viewer', now() + interval '1 day')`)
	if err := api.QueryRow(ctx, `SELECT auth_login_tenant($1)`, userOwnerB).Scan(&got); err != nil || got == nil || got.String() != tenantA {
		t.Fatalf("login tenant with a live membership: %v (err %v), want %s", got, err, tenantA)
	}
}
