// Package pgtest prepares real Postgres databases for integration tests. Tests are skipped
// unless TEST_DATABASE_ADMIN_URL points at a superuser on a scratch server, connected to the
// "postgres" database. The database and roles are created exactly as in production, through
// internal/dbbootstrap, and migrations run as maiak_owner.
//
// Roles are cluster-wide and test packages run in parallel, so every package uses the same
// fixed role passwords and bootstrap runs are serialised with an advisory lock.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/theechofive/maiak/backend/internal/dbbootstrap"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/migrations"
)

// EnvAdminURL names the environment variable with the admin connection URL.
const EnvAdminURL = "TEST_DATABASE_ADMIN_URL"

// Fixed role passwords for tests (scratch servers only).
const (
	OwnerPassword  = "test-owner-password-not-secret"
	APIPassword    = "test-api-password-not-secret"
	WorkerPassword = "test-worker-password-not-secret"
)

// Passwords returns the fixed test passwords in the order dbbootstrap generates them
// (owner, api, worker), for use as Options.GeneratePassword.
func Passwords() func() (string, error) {
	var mu sync.Mutex
	pws := []string{OwnerPassword, APIPassword, WorkerPassword}
	i := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		pw := pws[i%len(pws)]
		i++
		return pw, nil
	}
}

const testLockKey int64 = 0x6d61_6961_6b74_7374 // "maiaktst"

// AdminConfig returns the admin connection config for database "postgres", or skips the test.
func AdminConfig(t testing.TB) *pgx.ConnConfig {
	t.Helper()
	raw := os.Getenv(EnvAdminURL)
	if raw == "" {
		t.Skipf("%s is not set; skipping integration test", EnvAdminURL)
	}
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatalf("%s is invalid", EnvAdminURL)
	}
	cfg.Database = "postgres"
	return cfg
}

// Lock serialises bootstrap work across test packages until the test ends.
func Lock(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, AdminConfig(t))
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", testLockKey); err != nil {
		t.Fatalf("test lock: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
}

// Env is a migrated test database.
type Env struct {
	DBName    string
	OwnerURL  string
	APIURL    string
	WorkerURL string
	admin     *pgx.ConnConfig
}

// New drops and recreates database name through dbbootstrap and applies all migrations.
func New(t testing.TB, name string) *Env {
	t.Helper()
	admin := AdminConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	lockConn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = lockConn.Close(context.Background()) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", testLockKey); err != nil {
		t.Fatalf("test lock: %v", err)
	}
	if _, err := lockConn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		t.Fatalf("drop database %s: %v", name, err)
	}

	secrets := NewFakeSecrets()
	host, port := admin.Host, int(admin.Port)
	res, err := dbbootstrap.Run(ctx, dbbootstrap.Options{
		Admin: admin, Host: host, Port: port, DBName: name, SSLMode: SSLMode(),
		Namespace: "test", SecretName: "maiak-db", Secrets: secrets, GeneratePassword: Passwords(),
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !res.DatabaseCreated {
		t.Fatalf("bootstrap did not create database %s", name)
	}
	data, _, _ := secrets.Get(ctx, "test", "maiak-db")
	env := &Env{
		DBName:    name,
		OwnerURL:  string(data[dbbootstrap.KeyOwnerURL]),
		APIURL:    string(data[dbbootstrap.KeyAPIURL]),
		WorkerURL: string(data[dbbootstrap.KeyWorkerURL]),
		admin:     admin,
	}
	if _, err := env.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return env
}

// SSLMode returns the sslmode of the admin URL (default "prefer").
func SSLMode() string {
	u, err := url.Parse(os.Getenv(EnvAdminURL))
	if err == nil {
		if m := u.Query().Get("sslmode"); m != "" {
			return m
		}
	}
	return "prefer"
}

// Migrate applies migrations as maiak_owner and returns the number applied.
func (e *Env) Migrate(ctx context.Context) (int, error) {
	cfg, err := pgx.ParseConfig(e.OwnerURL)
	if err != nil {
		return 0, err
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(2)
	res, err := migrations.Up(ctx, sqlDB)
	return len(res), err
}

// Pool opens a pool for url (one of the role URLs) and closes it when the test ends.
func (e *Env) Pool(t testing.TB, url string, maxConns int32) *db.Pool {
	t.Helper()
	p, err := db.Open(context.Background(), url, db.Options{MaxConns: maxConns, ApplicationName: "maiak-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// Conn opens a single connection with url and closes it when the test ends.
func (e *Env) Conn(t testing.TB, url string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// AdminConn opens a superuser connection to the test database.
func (e *Env) AdminConn(t testing.TB) *pgx.Conn {
	t.Helper()
	cfg := e.admin.Copy()
	cfg.Database = e.DBName
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect admin to %s: %v", e.DBName, err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// FakeSecrets is an in-memory dbbootstrap.SecretStore.
type FakeSecrets struct {
	mu          sync.Mutex
	secrets     map[string]map[string][]byte
	annotations map[string]map[string]string
	Creates     int
	Updates     int
}

// NewFakeSecrets returns an empty store.
func NewFakeSecrets() *FakeSecrets {
	return &FakeSecrets{secrets: map[string]map[string][]byte{}, annotations: map[string]map[string]string{}}
}

// Get implements dbbootstrap.SecretStore.
func (f *FakeSecrets) Get(_ context.Context, ns, name string) (map[string][]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.secrets[ns+"/"+name]
	if !ok {
		return nil, false, nil
	}
	out := make(map[string][]byte, len(d))
	for k, v := range d {
		out[k] = append([]byte(nil), v...)
	}
	return out, true, nil
}

// Create implements dbbootstrap.SecretStore.
func (f *FakeSecrets) Create(_ context.Context, ns, name string, data map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.secrets[ns+"/"+name]; ok {
		return dbbootstrap.ErrSecretExists
	}
	f.Creates++
	f.secrets[ns+"/"+name] = data
	return nil
}

// CreateAnnotated implements secretsbootstrap.SecretStore.
func (f *FakeSecrets) CreateAnnotated(ctx context.Context, ns, name string, data map[string][]byte, annotations map[string]string) error {
	if err := f.Create(ctx, ns, name, data); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.annotations[ns+"/"+name] = annotations
	return nil
}

// Annotations returns the annotations a Secret was created with.
func (f *FakeSecrets) Annotations(ns, name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.annotations[ns+"/"+name]
}

// Delete removes a Secret (to simulate an owner deleting it).
func (f *FakeSecrets) Delete(ns, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, ns+"/"+name)
	delete(f.annotations, ns+"/"+name)
}

// Update implements dbbootstrap.SecretStore.
func (f *FakeSecrets) Update(_ context.Context, ns, name string, data map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.secrets[ns+"/"+name]
	if !ok {
		return fmt.Errorf("secret %s/%s not found", ns, name)
	}
	f.Updates++
	for k, v := range data {
		d[k] = append([]byte(nil), v...)
	}
	return nil
}

// Put stores a Secret directly (to test pre-existing Secrets).
func (f *FakeSecrets) Put(ns, name string, data map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[ns+"/"+name] = data
}

// MustExec runs statements on conn and fails the test on error.
func MustExec(t testing.TB, conn *pgx.Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := conn.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// DBName returns a per-package database name.
func DBName(suffix string) string { return fmt.Sprintf("maiak_it_%s", suffix) }
