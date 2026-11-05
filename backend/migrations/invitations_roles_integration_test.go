package migrations_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackInvitationsRoles undoes the T-01 migration (ADR-0102), so a test can replay an older
// migration (goose refuses to apply a missing older version).
var rollbackInvitationsRoles = []string{
	`DROP FUNCTION IF EXISTS org_delete_tenant(uuid, uuid, inet)`,
	`DROP FUNCTION IF EXISTS auth_user_organizations(uuid)`,
	`DROP FUNCTION IF EXISTS auth_invitation_lookup(bytea)`,
	`DROP TABLE IF EXISTS invitations`,
	`UPDATE plans SET limits = limits - 'max_members'`,
	`DELETE FROM goose_db_version WHERE version_id = 20261105120000`,
}

const (
	userA = "01920000-0000-7000-8000-0000000000a1"
	userB = "01920000-0000-7000-8000-0000000000b1"
)

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// inTenant runs fn as the api role in a transaction bound to tenant.
func inTenant(t *testing.T, conn *pgx.Conn, tenant string, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TestIntegrationInvitationsRoles checks the T-01 migration: the invitations table is isolated
// per tenant and insert-only plus update for the api, the lookup returns open invitations only,
// the organizations of a user and the owner-only organization delete.
func TestIntegrationInvitationsRoles(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("invitations"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'Alpha'), ('`+tenantB+`', 'b', 'Beta')`,
		`INSERT INTO users (id, email) VALUES ('`+userA+`', 'a@example.com'), ('`+userB+`', 'b@example.com')`,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ('`+tenantA+`', '`+userA+`', 'owner'), ('`+tenantB+`', '`+userA+`', 'viewer'),
			('`+tenantB+`', '`+userB+`', 'owner')`,
	)

	insert := func(tenant, id, token, role, extra string) error {
		return inTenant(t, api, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO invitations (id, tenant_id, role, token_hash, created_by, created_at, expires_at)
				VALUES ($1, $2, $3, $4, $5, now() - interval '1 hour', now() + interval '7 days')`, id, tenant, role, tokenHash(token), userA)
			if err == nil && extra != "" {
				_, err = tx.Exec(ctx, `UPDATE invitations SET `+extra+` WHERE id = $1`, id)
			}
			return err
		})
	}
	const (
		open     = "01920000-0000-7000-8000-00000000c001"
		expired  = "01920000-0000-7000-8000-00000000c002"
		accepted = "01920000-0000-7000-8000-00000000c003"
		revoked  = "01920000-0000-7000-8000-00000000c004"
		other    = "01920000-0000-7000-8000-00000000c005"
	)
	for _, c := range []struct{ id, token, extra string }{
		{open, "open", ""},
		{expired, "expired", "expires_at = now() - interval '1 minute'"},
		{accepted, "accepted", "accepted_at = now(), accepted_by = '" + userB + "'"},
		{revoked, "revoked", "revoked_at = now()"},
	} {
		if err := insert(tenantA, c.id, c.token, "member", c.extra); err != nil {
			t.Fatalf("insert %s: %v", c.token, err)
		}
	}

	t.Run("checks", func(t *testing.T) {
		if err := insert(tenantA, other, "owner-role", "owner", ""); err == nil || !strings.Contains(err.Error(), "invitations_role_check") {
			t.Fatalf("owner invitation: %v", err)
		}
		if err := insert(tenantA, other, "both", "member", "accepted_at = now(), accepted_by = '"+userB+"', revoked_at = now()"); err == nil ||
			!strings.Contains(err.Error(), "invitations_single_outcome") {
			t.Fatalf("accepted and revoked: %v", err)
		}
		if err := insert(tenantA, other, "open", "member", ""); err == nil || !strings.Contains(err.Error(), "invitations_token_hash_key") {
			t.Fatalf("duplicate token hash: %v", err)
		}
		// Another tenant's id cannot be written from tenant A's transaction (WITH CHECK).
		if err := insert(tenantA, other, "cross", "member", "tenant_id = '"+tenantB+"'"); err == nil {
			t.Fatal("moved an invitation to another tenant")
		}
	})

	t.Run("isolation and grants", func(t *testing.T) {
		var n int
		if err := inTenant(t, api, tenantB, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM invitations`).Scan(&n)
		}); err != nil || n != 0 {
			t.Fatalf("tenant B sees %d invitations of A, err=%v", n, err)
		}
		err := inTenant(t, api, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM invitations WHERE id = $1`, revoked)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("api deletes invitations: %v", err)
		}
		worker := env.Conn(t, env.WorkerURL)
		if _, err := worker.Exec(ctx, `SELECT 1 FROM invitations`); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("worker reads invitations: %v", err)
		}
	})

	t.Run("lookup", func(t *testing.T) {
		look := func(token string) (string, int) {
			rows, err := api.Query(ctx, `SELECT tenant_id::text, tenant_name, role FROM auth_invitation_lookup($1)`, tokenHash(token))
			if err != nil {
				t.Fatal(err)
			}
			got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
				var tenant, name, role string
				err := r.Scan(&tenant, &name, &role)
				return tenant + "/" + name + "/" + role, err
			})
			if err != nil {
				t.Fatal(err)
			}
			return strings.Join(got, ","), len(got)
		}
		if got, n := look("open"); n != 1 || got != tenantA+"/Alpha/member" {
			t.Fatalf("open: %q", got)
		}
		for _, tok := range []string{"expired", "accepted", "revoked", "unknown"} {
			if got, n := look(tok); n != 0 {
				t.Errorf("%s: %q, want nothing", tok, got)
			}
		}
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantA+`'`)
		if _, n := look("open"); n != 0 {
			t.Error("open invitation of a suspended tenant found")
		}
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'active' WHERE id = '`+tenantA+`'`)
	})

	t.Run("organizations of a user", func(t *testing.T) {
		rows, err := api.Query(ctx, `SELECT name, role FROM auth_user_organizations($1)`, userA)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
			var name, role string
			err := r.Scan(&name, &role)
			return name + "=" + role, err
		})
		if err != nil || strings.Join(got, ",") != "Alpha=owner,Beta=viewer" {
			t.Fatalf("organizations = %v, %v", got, err)
		}
	})

	t.Run("organization delete", func(t *testing.T) {
		del := func(tenant, actor string) (bool, error) {
			var ok bool
			err := inTenant(t, api, tenant, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT org_delete_tenant($1, gen_random_uuid(), NULL)`, actor).Scan(&ok)
			})
			return ok, err
		}
		// A viewer of B is refused; so is anyone acting on the default tenant.
		if _, err := del(tenantB, userA); err == nil || !strings.Contains(err.Error(), "MKO02") {
			t.Fatalf("viewer deletes: %v", err)
		}
		pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('01920000-0000-7000-8000-00000000000d', 'default', 'Default')`,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ('01920000-0000-7000-8000-00000000000d', '`+userB+`', 'owner')`)
		if _, err := del("01920000-0000-7000-8000-00000000000d", userB); err == nil || !strings.Contains(err.Error(), "MKO01") {
			t.Fatalf("default tenant deleted: %v", err)
		}
		if ok, err := del(tenantB, userB); err != nil || !ok {
			t.Fatalf("owner delete: %v %v", ok, err)
		}
		if ok, err := del(tenantB, userB); err != nil || ok {
			t.Fatalf("second delete: %v %v", ok, err)
		}
		var status string
		var audits int
		if err := admin.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM audit_log WHERE tenant_id = t.id AND action = 'organization.deleted')
			FROM tenants AS t WHERE id = $1 AND deleted_at IS NOT NULL`, tenantB).Scan(&status, &audits); err != nil || status != "suspended" || audits != 1 {
			t.Fatalf("deleted tenant: %s audits=%d err=%v", status, audits, err)
		}
	})
}
