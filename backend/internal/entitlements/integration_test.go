package entitlements_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/entitlements"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationFeatures checks that a tenant's features are its plan's, sorted and deduplicated,
// and that the seeded plans unlock nothing (O-08, ADR-0105).
func TestIntegrationFeatures(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("entitlements"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	svc := entitlements.NewService(env.Pool(t, env.APIURL, 2))
	free, community := uuid.New(), uuid.New()
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+free.String()+`', 'free-org', 'Free')`,
		`INSERT INTO tenants (id, slug, name, plan_key) VALUES ('`+community.String()+`', 'community-org', 'Community', 'community')`)

	for _, id := range []uuid.UUID{free, community} {
		got, err := svc.Features(ctx, id)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("seeded plan features of %s = %#v (%v), want empty", id, got, err)
		}
	}

	pgtest.MustExec(t, admin, `UPDATE plans SET features = '{sso,audit-export,sso}' WHERE key = 'free'`)
	got, err := svc.Features(ctx, free)
	if err != nil || !slices.Equal(got, []string{"audit-export", "sso"}) {
		t.Fatalf("features = %v (%v), want [audit-export sso]", got, err)
	}
	if ok, err := svc.Has(ctx, free, "sso"); err != nil || !ok {
		t.Fatalf("Has(sso) = %t (%v), want true", ok, err)
	}
	if ok, err := svc.Has(ctx, community, "sso"); err != nil || ok {
		t.Fatalf("community Has(sso) = %t (%v), want false", ok, err)
	}
	if _, err := svc.Features(ctx, uuid.New()); err == nil {
		t.Fatal("features of an unknown tenant: no error")
	}
	if _, err := admin.Exec(ctx, `UPDATE plans SET features = '{NULL}' WHERE key = 'free'`); err == nil {
		t.Fatal("a NULL feature name was accepted")
	}
}
