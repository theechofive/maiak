package dbbootstrap_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/theechofive/maiak/backend/internal/dbbootstrap"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

func TestIntegrationBootstrapTwice(t *testing.T) {
	admin := pgtest.AdminConfig(t)
	pgtest.Lock(t)
	ctx := context.Background()
	name := pgtest.DBName("bootstrap")

	adminConn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminConn.Close(ctx) }()
	pgtest.MustExec(t, adminConn, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")

	secrets := pgtest.NewFakeSecrets()
	opts := func(out *bytes.Buffer) dbbootstrap.Options {
		return dbbootstrap.Options{
			Admin: admin, Host: admin.Host, Port: int(admin.Port), DBName: name, SSLMode: pgtest.SSLMode(),
			Namespace: "maiak", SecretName: "maiak-db", Secrets: secrets,
			// The fixed test passwords, so parallel test packages keep working.
			GeneratePassword: pgtest.Passwords(),
			Out:              out,
		}
	}

	var out1 bytes.Buffer
	res, err := dbbootstrap.Run(ctx, opts(&out1))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !res.SecretCreated || !res.DatabaseCreated || res.DatabaseOwner != "maiak_owner" || secrets.Creates != 1 {
		t.Fatalf("first run result: %+v creates=%d", res, secrets.Creates)
	}

	data, found, _ := secrets.Get(ctx, "maiak", "maiak-db")
	if !found || len(data) != 6 {
		t.Fatalf("secret keys: %d", len(data))
	}
	for _, pw := range []string{pgtest.OwnerPassword, pgtest.APIPassword, pgtest.WorkerPassword} {
		if strings.Contains(out1.String(), pw) {
			t.Fatal("summary output contains a password")
		}
	}
	if !strings.Contains(out1.String(), "maiak_api") || !strings.Contains(out1.String(), "statement_timeout=15s") {
		t.Fatalf("summary:\n%s", out1.String())
	}

	var out2 bytes.Buffer
	res2, err := dbbootstrap.Run(ctx, opts(&out2))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res2.SecretCreated || res2.DatabaseCreated || secrets.Creates != 1 || secrets.Updates != 0 {
		t.Fatalf("second run must reuse everything: %+v creates=%d updates=%d", res2, secrets.Creates, secrets.Updates)
	}
	if res2.AdminOwnerMember || !strings.Contains(out2.String(), "admin member of maiak_owner: false") {
		t.Fatalf("admin membership not reported as revoked:\n%s", out2.String())
	}

	// A Secret with a stale and a missing URL is repaired, not passed through; passwords stay.
	stale := map[string][]byte{}
	for k, v := range data {
		stale[k] = v
	}
	stale[dbbootstrap.KeyAPIURL] = []byte("postgres://maiak_api:old@old-host:5432/maiak?sslmode=disable")
	delete(stale, dbbootstrap.KeyWorkerURL)
	secrets.Put("maiak", "maiak-db", stale)
	var logs bytes.Buffer
	o3 := opts(&bytes.Buffer{})
	o3.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := dbbootstrap.Run(ctx, o3); err != nil {
		t.Fatalf("run with stale URLs: %v", err)
	}
	repaired, _, _ := secrets.Get(ctx, "maiak", "maiak-db")
	if secrets.Updates != 1 || secrets.Creates != 1 {
		t.Fatalf("updates=%d creates=%d, want 1/1", secrets.Updates, secrets.Creates)
	}
	for k, v := range data {
		if string(repaired[k]) != string(v) {
			t.Errorf("%s not restored", k)
		}
	}
	if !strings.Contains(logs.String(), "api_url") || !strings.Contains(logs.String(), "worker_url") {
		t.Fatalf("drift notice missing:\n%s", logs.String())
	}
	for _, pw := range []string{pgtest.OwnerPassword, pgtest.APIPassword, pgtest.WorkerPassword, "old-host"} {
		if strings.Contains(logs.String(), pw) {
			t.Fatal("drift notice contains a secret or URL")
		}
	}

	for _, ri := range res2.Roles {
		if ri.Superuser || ri.BypassRLS || ri.Replication || ri.CreateDB || ri.CreateRole || !ri.CanLogin {
			t.Errorf("%s has unsafe attributes: %+v", ri.Name, ri)
		}
		want := map[string]int{"maiak_owner": 3, "maiak_api": 20, "maiak_worker": 20}[ri.Name]
		if ri.ConnLimit != want {
			t.Errorf("%s connection limit = %d, want %d", ri.Name, ri.ConnLimit, want)
		}
	}

	// The admin's temporary membership in maiak_owner is gone.
	var member bool
	if err := adminConn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_auth_members m
		WHERE m.roleid = 'maiak_owner'::regrole AND m.member = (SELECT oid FROM pg_roles WHERE rolname = current_user))`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	if member {
		t.Fatal("admin is still a member of maiak_owner")
	}

	// Every role logs in with the URL from the Secret (proves the SCRAM verifier).
	for _, key := range []string{dbbootstrap.KeyOwnerURL, dbbootstrap.KeyAPIURL, dbbootstrap.KeyWorkerURL} {
		conn, err := pgx.Connect(ctx, string(data[key]))
		if err != nil {
			t.Fatalf("%s: connect: %v", key, err)
		}
		var user string
		if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&user); err != nil {
			t.Fatal(err)
		}
		_, createErr := conn.Exec(ctx, "CREATE TABLE bootstrap_probe (id int)")
		_ = conn.Close(ctx)
		var pgErr *pgconn.PgError
		denied := createErr != nil && errors.As(createErr, &pgErr) && pgErr.Code == "42501"
		if user == "maiak_owner" && createErr != nil {
			t.Errorf("owner cannot create tables: %v", createErr)
		}
		if user != "maiak_owner" && !denied {
			t.Errorf("%s can create tables in public (err=%v)", user, createErr)
		}
	}

	// Database and schema privileges.
	dbConn := pgtestConn(t, admin, name)
	var publicConnect, publicCreate bool
	var schemaOwner string
	if err := dbConn.QueryRow(ctx, `SELECT has_database_privilege('public', current_database(), 'CONNECT'),
		has_schema_privilege('public', 'public', 'CREATE'), pg_get_userbyid(nspowner)
		FROM pg_namespace WHERE nspname = 'public'`).Scan(&publicConnect, &publicCreate, &schemaOwner); err != nil {
		t.Fatal(err)
	}
	if publicConnect || publicCreate || schemaOwner != "maiak_owner" {
		t.Fatalf("PUBLIC connect=%v create=%v schema owner=%s", publicConnect, publicCreate, schemaOwner)
	}
	var citext bool
	if err := dbConn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'citext')`).Scan(&citext); err != nil || !citext {
		t.Fatalf("citext extension: %v %v", citext, err)
	}
}

func TestIntegrationBootstrapRefusesIncompleteSecret(t *testing.T) {
	admin := pgtest.AdminConfig(t)
	secrets := pgtest.NewFakeSecrets()
	secrets.Put("maiak", "maiak-db", map[string][]byte{"api_url": []byte("postgres://x")})
	_, err := dbbootstrap.Run(context.Background(), dbbootstrap.Options{
		Admin: admin, Host: admin.Host, Port: int(admin.Port), DBName: pgtest.DBName("bootstrap_refused"), SSLMode: "disable",
		Namespace: "maiak", SecretName: "maiak-db", Secrets: secrets,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v", err)
	}
	if secrets.Creates != 0 {
		t.Fatal("incomplete secret must not be replaced")
	}
}

func pgtestConn(t *testing.T, admin *pgx.ConnConfig, db string) *pgx.Conn {
	t.Helper()
	cfg := admin.Copy()
	cfg.Database = db
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// TestIntegrationBootstrapNonSuperuser runs the bootstrap twice as a NOSUPERUSER CREATEROLE
// CREATEDB login, an approximation of the RDS master user (a member of rds_superuser).
//
// RDS differs and is NOT verified here (L-022): rds_superuser has extra grants (pg_signal_backend,
// rds-specific roles), RDS blocks some catalogs and parameters, and PostgreSQL 15 lets CREATEROLE
// alter any non-superuser role, while PostgreSQL 16+ requires ADMIN OPTION on the role. To mimic
// PostgreSQL 15 on a newer test server, the test admin is given ADMIN OPTION (no INHERIT, no SET)
// on the three roles, which is what PostgreSQL 16 grants a CREATEROLE creator implicitly.
func TestIntegrationBootstrapNonSuperuser(t *testing.T) {
	super := pgtest.AdminConfig(t)
	pgtest.Lock(t)
	ctx := context.Background()
	name := pgtest.DBName("bootstrap_nosuper")
	const adminRole = "maiak_it_nosuper"

	superConn, err := pgx.ConnectConfig(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = superConn.Close(ctx) }) // registered first, so it runs after cleanup
	var version int
	if err := superConn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		pgtest.MustExec(t, superConn, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		var exists bool
		_ = superConn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", adminRole).Scan(&exists)
		if exists {
			// Memberships it granted (PG16+ records the grantor) go with DROP OWNED.
			pgtest.MustExec(t, superConn, "DROP OWNED BY "+adminRole, "DROP ROLE "+adminRole)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	pgtest.MustExec(t, superConn,
		"CREATE ROLE "+adminRole+" LOGIN NOSUPERUSER CREATEROLE CREATEDB NOREPLICATION NOBYPASSRLS PASSWORD 'not-secret-test-only'")
	if version >= 160000 {
		for _, r := range []string{dbbootstrap.OwnerRole, dbbootstrap.APIRole, dbbootstrap.WorkerRole} {
			var exists bool
			if err := superConn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", r).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists {
				pgtest.MustExec(t, superConn, "GRANT "+r+" TO "+adminRole+" WITH ADMIN OPTION, INHERIT FALSE, SET FALSE")
			}
		}
	}

	admin := super.Copy()
	admin.User, admin.Password = adminRole, "not-secret-test-only"
	secrets := pgtest.NewFakeSecrets()
	for i := 1; i <= 2; i++ {
		var out bytes.Buffer
		res, err := dbbootstrap.Run(ctx, dbbootstrap.Options{
			Admin: admin, Host: admin.Host, Port: int(admin.Port), DBName: name, SSLMode: pgtest.SSLMode(),
			Namespace: "maiak", SecretName: "maiak-db", Secrets: secrets, GeneratePassword: pgtest.Passwords(), Out: &out,
		})
		if err != nil {
			t.Fatalf("run %d as %s: %v", i, adminRole, err)
		}
		if res.DatabaseOwner != dbbootstrap.OwnerRole || res.DatabaseCreated != (i == 1) {
			t.Fatalf("run %d: %+v", i, res)
		}
		t.Logf("run %d summary:\n%s", i, out.String())
	}

	// The temporary SET/INHERIT membership is gone: the admin cannot act as maiak_owner.
	var usage bool
	if err := superConn.QueryRow(ctx, "SELECT pg_has_role($1, 'maiak_owner', 'USAGE')", adminRole).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage {
		t.Fatalf("%s still has the privileges of maiak_owner", adminRole)
	}

	// The database works for the owner and is closed to PUBLIC.
	dbConn := pgtestConn(t, super, name)
	var publicConnect bool
	var schemaOwner, citextOwner string
	if err := dbConn.QueryRow(ctx, `SELECT has_database_privilege('public', current_database(), 'CONNECT'),
		(SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'public'),
		(SELECT pg_get_userbyid(extowner) FROM pg_extension WHERE extname = 'citext')`).Scan(&publicConnect, &schemaOwner, &citextOwner); err != nil {
		t.Fatal(err)
	}
	if publicConnect || schemaOwner != dbbootstrap.OwnerRole || citextOwner != dbbootstrap.OwnerRole {
		t.Fatalf("PUBLIC connect=%v schema owner=%s citext owner=%s", publicConnect, schemaOwner, citextOwner)
	}
}
