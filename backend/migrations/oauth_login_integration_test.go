package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackOAuthLogin undoes the O-01 migration (ADR-0104), so a test can replay an older migration
// (goose refuses to apply a missing older version).
var rollbackOAuthLogin = []string{
	`DROP FUNCTION IF EXISTS auth_domain_lookup(text)`,
	`DROP TABLE IF EXISTS org_auth_domains`,
	`DROP INDEX IF EXISTS auth_identities_user_provider_key`,
	`ALTER TABLE auth_identities DROP CONSTRAINT IF EXISTS auth_identities_known_provider,
		DROP COLUMN IF EXISTS email, DROP COLUMN IF EXISTS hosted_domain, DROP COLUMN IF EXISTS last_login_at`,
	`ALTER TABLE users DROP COLUMN IF EXISTS email_verified_at`,
	`DELETE FROM goose_db_version WHERE version_id = 20261106180000`,
}

// TestIntegrationOAuthLogin checks the O-01 migration (ADR-0104): the api's grants on users and
// auth_identities, the tenant isolation and global uniqueness of org_auth_domains, and the domain
// lookup that runs before the tenant is known.
func TestIntegrationOAuthLogin(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("oauthlogin"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'Alpha'), ('`+tenantB+`', 'b', 'Beta')`,
		`INSERT INTO users (id, email) VALUES ('`+userA+`', 'a@example.com'), ('`+userB+`', 'b@example.com')`,
	)
	exec := func(sql string, args ...any) error {
		_, err := api.Exec(ctx, sql, args...)
		return err
	}
	denied := func(err error) bool { return err != nil && strings.Contains(err.Error(), "permission denied") }

	// users: the api inserts a verified OAuth user and marks an email verified, nothing more.
	if err := exec(`INSERT INTO users (id, email, display_name, email_verified_at) VALUES ('01920000-0000-7000-8000-0000000000c1', 'c@example.com', 'C', now())`); err != nil {
		t.Fatalf("insert oauth user: %v", err)
	}
	if err := exec(`UPDATE users SET email_verified_at = now() WHERE id = $1`, userA); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	if err := exec(`INSERT INTO users (id, email, is_platform_admin) VALUES ('01920000-0000-7000-8000-0000000000c2', 'd@example.com', true)`); !denied(err) {
		t.Fatalf("insert with is_platform_admin: %v", err)
	}

	// auth_identities: insert, refresh email/hd/last login, delete; never rewrite subject or owner.
	if err := exec(`INSERT INTO auth_identities (user_id, provider, subject, email, hosted_domain, last_login_at) VALUES ($1, 'google', 's1', 'a@example.com', 'example.com', now())`, userA); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	if err := exec(`UPDATE auth_identities SET email = 'x@example.com', hosted_domain = NULL, last_login_at = now() WHERE subject = 's1'`); err != nil {
		t.Fatalf("refresh identity: %v", err)
	}
	for _, sql := range []string{
		`UPDATE auth_identities SET subject = 's2' WHERE subject = 's1'`,
		`UPDATE auth_identities SET user_id = '` + userB + `' WHERE subject = 's1'`,
	} {
		if err := exec(sql); !denied(err) {
			t.Errorf("%s: %v", sql, err)
		}
	}
	if err := exec(`INSERT INTO auth_identities (user_id, provider, subject) VALUES ($1, 'google', 's9')`, userA); err == nil {
		t.Fatal("a second google identity for one user was accepted")
	}
	if err := exec(`INSERT INTO auth_identities (user_id, provider, subject) VALUES ($1, 'gitlab', 's3')`, userB); err == nil {
		t.Fatal("unknown provider accepted")
	}

	// org_auth_domains: tenant isolated, one organization per domain.
	insertDomain := func(tenant, id, domain string, enabled bool) error {
		return inTenant(t, api, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO org_auth_domains (id, tenant_id, domain, enabled, created_by) VALUES ($1, $2, $3, $4, $5)`,
				id, tenant, domain, enabled, userA)
			return err
		})
	}
	if err := insertDomain(tenantA, "01920000-0000-7000-8000-0000000000d1", "example.com", true); err != nil {
		t.Fatal(err)
	}
	if err := insertDomain(tenantB, "01920000-0000-7000-8000-0000000000d2", "example.com", true); err == nil {
		t.Fatal("the same domain was claimed twice")
	}
	if err := insertDomain(tenantB, "01920000-0000-7000-8000-0000000000d3", "off.example", false); err != nil {
		t.Fatal(err)
	}
	if err := insertDomain(tenantA, "01920000-0000-7000-8000-0000000000d4", "Example.ORG", true); err == nil {
		t.Fatal("an uppercase domain passed the check")
	}
	if err := inTenant(t, api, tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO org_auth_domains (id, tenant_id, domain, created_by) VALUES ('01920000-0000-7000-8000-0000000000d6', $1, 'y.org', $2)`, tenantB, userA)
		return err
	}); err == nil {
		t.Fatal("insert into another tenant passed RLS")
	}
	var visible int
	if err := inTenant(t, api, tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM org_auth_domains`).Scan(&visible)
	}); err != nil || visible != 1 {
		t.Fatalf("tenant B sees %d domains (%v), want its own 1", visible, err)
	}

	// The lookup answers enabled domains of active tenants only, for an exact domain.
	lookup := func(domain string) string {
		var tenant string
		err := api.QueryRow(ctx, `SELECT tenant_id::text FROM auth_domain_lookup($1)`, domain).Scan(&tenant)
		if errors.Is(err, pgx.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return tenant
	}
	if got := lookup("example.com"); got != tenantA {
		t.Fatalf("lookup example.com = %q", got)
	}
	for _, d := range []string{"off.example", "EXAMPLE.COM", "com", ""} {
		if got := lookup(d); got != "" {
			t.Errorf("lookup %q = %q, want none", d, got)
		}
	}
	pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantA+`'`)
	if got := lookup("example.com"); got != "" {
		t.Fatalf("lookup of a suspended tenant's domain = %q", got)
	}
	// The worker has no business with any of it.
	worker := env.Conn(t, env.WorkerURL)
	if _, err := worker.Exec(ctx, `SELECT * FROM auth_domain_lookup('example.com')`); !denied(err) {
		t.Fatalf("worker calls the lookup: %v", err)
	}
}
