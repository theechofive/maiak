package migrations_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationAuthLoginTenant checks the registration migration (P-02): auth_login_tenant
// returns the user's oldest membership in a live tenant (suspended included, deleted skipped)
// or NULL, and the api still cannot read memberships without the tenant set.
func TestIntegrationAuthLoginTenant(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("logintenant"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	const (
		userOne  = "01920000-0000-7000-8000-0000000000c1"
		userNone = "01920000-0000-7000-8000-0000000000c2"
		tenantC  = "01920000-0000-7000-8000-00000000000c"
	)
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B'), ('`+tenantC+`', 'c', 'C')`,
		`INSERT INTO users (id, email) VALUES ('`+userOne+`', 'one@example.com'), ('`+userNone+`', 'none@example.com')`,
		`INSERT INTO memberships (tenant_id, user_id, role, created_at) VALUES
			('`+tenantB+`', '`+userOne+`', 'owner', now() - interval '2 days'),
			('`+tenantA+`', '`+userOne+`', 'owner', now() - interval '1 day'),
			('`+tenantC+`', '`+userOne+`', 'owner', now())`)

	api := env.Conn(t, env.APIURL)
	loginTenant := func(user string) *uuid.UUID {
		t.Helper()
		var id *uuid.UUID
		if err := api.QueryRow(ctx, `SELECT auth_login_tenant($1)`, user).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if got := loginTenant(userOne); got == nil || got.String() != tenantB {
		t.Fatalf("oldest membership: %v, want %s", got, tenantB)
	}
	pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantB+`'`)
	if got := loginTenant(userOne); got == nil || got.String() != tenantB {
		t.Fatalf("suspended tenant is returned (the login refuses it): %v", got)
	}
	pgtest.MustExec(t, admin, `UPDATE tenants SET deleted_at = now() WHERE id = '`+tenantB+`'`)
	if got := loginTenant(userOne); got == nil || got.String() != tenantA {
		t.Fatalf("deleted tenant skipped: %v, want %s", got, tenantA)
	}
	if got := loginTenant(userNone); got != nil {
		t.Fatalf("user without membership: %v, want NULL", got)
	}
	if got := loginTenant(uuid.NewString()); got != nil {
		t.Fatalf("unknown user: %v, want NULL", got)
	}

	// The owner-only policy does not widen what the runtime roles see.
	var visible int
	if err := api.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("api sees %d memberships without a tenant, err=%v", visible, err)
	}
}
