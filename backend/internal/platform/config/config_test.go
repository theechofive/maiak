package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/platform/crypto"
)

func env(m map[string]string) LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func validHash(t *testing.T) string {
	t.Helper()
	h, err := crypto.HashPassword("pw", crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func apiEnv(t *testing.T) map[string]string {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	return map[string]string{
		"MAIAK_DATABASE_URL":             "postgres://maiak_api:pw@db:5432/maiak?sslmode=require",
		"MAIAK_ADMIN_EMAIL":              "admin@example.com",
		"MAIAK_ADMIN_PASSWORD_HASH":      validHash(t),
		"MAIAK_ENCRYPTION_KEYS":          fmt.Sprintf(`{"k1":%q}`, key),
		"MAIAK_ENCRYPTION_ACTIVE_KEY_ID": "k1",
		"MAIAK_PUBLIC_URL":               "https://maiak.example.com",
	}
}

func TestLoadAPIDefaults(t *testing.T) {
	cfg, err := LoadAPI(env(apiEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "production" || cfg.LogLevel != slog.LevelInfo || cfg.HTTPAddr != ":8080" || cfg.AdminAddr != ":9090" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.SessionTTL != 12*time.Hour || !cfg.CookieSecure || cfg.TrustedProxyHops != 1 {
		t.Fatalf("unexpected auth defaults: ttl=%v secure=%v hops=%d", cfg.SessionTTL, cfg.CookieSecure, cfg.TrustedProxyHops)
	}
	if cfg.ShutdownTimeout != 25*time.Second {
		t.Fatalf("shutdown timeout = %v", cfg.ShutdownTimeout)
	}
	if len(cfg.EncryptionKeys["k1"]) != 32 || cfg.EncryptionKeyID != "k1" {
		t.Fatal("encryption keys not decoded")
	}
}

func TestLoadAPIOverrides(t *testing.T) {
	e := apiEnv(t)
	e["MAIAK_ENV"] = "staging"
	e["MAIAK_LOG_LEVEL"] = "debug"
	e["MAIAK_SESSION_TTL"] = "24h"
	e["MAIAK_COOKIE_SECURE"] = "false"
	e["MAIAK_TRUSTED_PROXY_HOPS"] = "0"
	e["MAIAK_HTTP_ADDR"] = "127.0.0.1:8081"
	cfg, err := LoadAPI(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "staging" || cfg.LogLevel != slog.LevelDebug || cfg.SessionTTL != 24*time.Hour || cfg.CookieSecure || cfg.TrustedProxyHops != 0 || cfg.HTTPAddr != "127.0.0.1:8081" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestLoadAPIReportsAllErrors(t *testing.T) {
	_, err := LoadAPI(env(map[string]string{
		"MAIAK_ENV":                      "prod",
		"MAIAK_LOG_LEVEL":                "loud",
		"MAIAK_ADMIN_EMAIL":              "Admin <admin@example.com>",
		"MAIAK_ADMIN_PASSWORD_HASH":      "plaintext-password",
		"MAIAK_SESSION_TTL":              "forever",
		"MAIAK_COOKIE_SECURE":            "maybe",
		"MAIAK_TRUSTED_PROXY_HOPS":       "-1",
		"MAIAK_ENCRYPTION_KEYS":          `{"k1":"c2hvcnQ="}`,
		"MAIAK_ENCRYPTION_ACTIVE_KEY_ID": "k1",
		"MAIAK_ADMIN_ADDR":               "9090",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, name := range []string{
		"MAIAK_ENV", "MAIAK_LOG_LEVEL", "MAIAK_ADMIN_EMAIL", "MAIAK_ADMIN_PASSWORD_HASH", "MAIAK_SESSION_TTL",
		"MAIAK_COOKIE_SECURE", "MAIAK_TRUSTED_PROXY_HOPS", "MAIAK_ENCRYPTION_KEYS", "MAIAK_DATABASE_URL", "MAIAK_ADMIN_ADDR",
	} {
		if !strings.Contains(msg, name) {
			t.Errorf("error does not mention %s:\n%s", name, msg)
		}
	}
	if strings.Contains(msg, "plaintext-password") {
		t.Error("error message leaks the password hash value")
	}
}

func TestDatabaseURLErrorDoesNotLeak(t *testing.T) {
	e := apiEnv(t)
	e["MAIAK_DATABASE_URL"] = "postgres://u:supersecret@host:notaport/db"
	_, err := LoadAPI(env(e))
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncryptionActiveKeyMustExist(t *testing.T) {
	e := apiEnv(t)
	e["MAIAK_ENCRYPTION_ACTIVE_KEY_ID"] = "k2"
	if _, err := LoadAPI(env(e)); err == nil || !strings.Contains(err.Error(), "MAIAK_ENCRYPTION_KEYS") {
		t.Fatalf("err = %v", err)
	}
}

func TestSecretRedaction(t *testing.T) {
	s := Secret("hunter2")
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %s %#v", s, s, s), fmt.Sprintf("%+v", struct{ S Secret }{s})} {
		if strings.Contains(out, "hunter2") {
			t.Fatalf("secret leaked: %s", out)
		}
	}
	k := EncryptionKeys{"k1": []byte("0123456789abcdef0123456789abcdef")}
	if out := fmt.Sprintf("%v %+v %#v", k, k, k); strings.Contains(out, "0123") {
		t.Fatalf("keys leaked: %s", out)
	}
	if s.Value() != "hunter2" {
		t.Fatal("Value() must return the secret")
	}
}

func TestLoadWorkerAndMigrate(t *testing.T) {
	e := apiEnv(t)
	if _, err := LoadWorker(env(e)); err != nil {
		t.Fatalf("worker: %v", err)
	}
	// The worker never needs the encryption keys (ADR-0016, L-042).
	delete(e, "MAIAK_ENCRYPTION_KEYS")
	delete(e, "MAIAK_ENCRYPTION_ACTIVE_KEY_ID")
	if _, err := LoadWorker(env(e)); err != nil {
		t.Fatalf("worker without encryption keys: %v", err)
	}
	if _, err := LoadMigrate(env(map[string]string{})); err == nil || !strings.Contains(err.Error(), "MAIAK_MIGRATE_DATABASE_URL") {
		t.Fatalf("migrate without URL: %v", err)
	}
	m, err := LoadMigrate(env(map[string]string{"MAIAK_MIGRATE_DATABASE_URL": "postgres://maiak_owner:pw@db/maiak"}))
	if err != nil || m.DatabaseURL.Value() == "" {
		t.Fatalf("migrate: %+v %v", m, err)
	}
}

func TestLoadJobs(t *testing.T) {
	e := apiEnv(t)
	cfg, err := LoadJobs(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.EncryptionKeys["k1"]) != 32 || cfg.EncryptionKeyID != "k1" || cfg.DatabaseMaxConns != 5 {
		t.Fatalf("jobs config = %+v", cfg)
	}
	delete(e, "MAIAK_ENCRYPTION_KEYS")
	if _, err := LoadJobs(env(e)); err == nil || !strings.Contains(err.Error(), "MAIAK_ENCRYPTION_KEYS") {
		t.Fatalf("jobs without encryption keys: %v", err)
	}
}

// Every process using the guarded client reads MAIAK_BLOCKED_CIDRS (ADR-0018).
func TestBlockedCIDRsInEveryGuardedProcess(t *testing.T) {
	loaders := map[string]func(LookupFunc) (Guard, error){
		"api": func(l LookupFunc) (Guard, error) { c, err := LoadAPI(l); return c.Guard, err },
		"worker": func(l LookupFunc) (Guard, error) {
			c, err := LoadWorker(l)
			return c.Guard, err
		},
		"jobs": func(l LookupFunc) (Guard, error) { c, err := LoadJobs(l); return c.Guard, err },
	}
	for name, load := range loaders {
		t.Run(name, func(t *testing.T) {
			e := apiEnv(t)
			e["MAIAK_BLOCKED_CIDRS"] = " 203.0.113.7/24, ,2001:db8::/32 "
			cfg, err := load(env(e))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.BlockedCIDRs) != 2 || cfg.BlockedCIDRs[0].String() != "203.0.113.0/24" || cfg.BlockedCIDRs[1].String() != "2001:db8::/32" {
				t.Fatalf("blocked cidrs = %v", cfg.BlockedCIDRs)
			}
			e["MAIAK_BLOCKED_CIDRS"] = "10.0.0.0/8,not-a-cidr"
			if _, err := load(env(e)); err == nil || !strings.Contains(err.Error(), "MAIAK_BLOCKED_CIDRS") {
				t.Fatalf("invalid cidr accepted: %v", err)
			}
		})
	}
}

func TestLoadWorkerLimits(t *testing.T) {
	e := apiEnv(t)
	cfg, err := LoadWorker(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaimBatch != 100 || cfg.Concurrency != 50 || cfg.TenantConcurrency != 5 || cfg.HostConcurrency != 2 || cfg.Location != "home-eu-central-1" {
		t.Fatalf("defaults = %d %d %d %d %q", cfg.ClaimBatch, cfg.Concurrency, cfg.TenantConcurrency, cfg.HostConcurrency, cfg.Location)
	}
	e["MAIAK_WORKER_LOCATION"] = "Home_EU"
	if _, err := LoadWorker(env(e)); err == nil || !strings.Contains(err.Error(), "MAIAK_WORKER_LOCATION") {
		t.Fatalf("invalid location accepted: %v", err)
	}
	e["MAIAK_WORKER_LOCATION"] = "aws-us-east-1"
	if cfg, err := LoadWorker(env(e)); err != nil || cfg.Location != "aws-us-east-1" {
		t.Fatalf("location = %q, err %v", cfg.Location, err)
	}
	e["MAIAK_WORKER_CONCURRENCY"] = "20"
	e["MAIAK_WORKER_HOST_CONCURRENCY"] = "0"
	if _, err := LoadWorker(env(e)); err == nil || !strings.Contains(err.Error(), "MAIAK_WORKER_HOST_CONCURRENCY") {
		t.Fatalf("zero host concurrency accepted: %v", err)
	}
}

func TestLoadBootstrap(t *testing.T) {
	cfg, err := LoadBootstrap(env(map[string]string{
		"MAIAK_BOOTSTRAP_HOST":           "db.example.internal",
		"MAIAK_BOOTSTRAP_ADMIN_USER":     "postgres",
		"MAIAK_BOOTSTRAP_ADMIN_PASSWORD": "pw",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 5432 || cfg.DBName != "maiak" || cfg.SSLMode != "require" || cfg.SecretName != "maiak-db" || cfg.Namespace != "" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	_, err = LoadBootstrap(env(map[string]string{
		"MAIAK_BOOTSTRAP_PORT":        "70000",
		"MAIAK_BOOTSTRAP_DB_NAME":     "Maiak; DROP",
		"MAIAK_BOOTSTRAP_SSLMODE":     "off",
		"MAIAK_BOOTSTRAP_SECRET_NAME": "Bad_Name",
		"MAIAK_BOOTSTRAP_NAMESPACE":   "-ns",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, name := range []string{"HOST", "ADMIN_USER", "ADMIN_PASSWORD", "PORT", "DB_NAME", "SSLMODE", "SECRET_NAME", "NAMESPACE"} {
		if !strings.Contains(err.Error(), "MAIAK_BOOTSTRAP_"+name) {
			t.Errorf("error does not mention MAIAK_BOOTSTRAP_%s", name)
		}
	}
}

// MAIAK_PUBLIC_URL is linked from Slack messages as <url/monitors/id|text>, so it must be a plain
// https URL without a trailing slash or characters that break the link.
func TestPublicURL(t *testing.T) {
	loaders := map[string]func(LookupFunc) (string, error){
		"api":  func(l LookupFunc) (string, error) { c, err := LoadAPI(l); return c.PublicURL, err },
		"jobs": func(l LookupFunc) (string, error) { c, err := LoadJobs(l); return c.PublicURL, err },
	}
	for name, load := range loaders {
		t.Run(name, func(t *testing.T) {
			e := apiEnv(t)
			for _, ok := range []string{"https://maiak.example.com", "https://maiak.klr-ops.net/app", "https://host:8443"} {
				e["MAIAK_PUBLIC_URL"] = ok
				if got, err := load(env(e)); err != nil || got != ok {
					t.Errorf("%q: got %q, %v", ok, got, err)
				}
			}
			for _, bad := range []string{"", "http://maiak.example.com", "https://maiak.example.com/", "maiak.example.com",
				"https://user:pw@maiak.example.com", "https://maiak.example.com?x=1", "https://maiak.example.com#a",
				"https://maiak.example.com/a|b", "https://maiak.example.com/<x>", "https://"} {
				e["MAIAK_PUBLIC_URL"] = bad
				if _, err := load(env(e)); err == nil || !strings.Contains(err.Error(), "MAIAK_PUBLIC_URL") {
					t.Errorf("%q accepted: %v", bad, err)
				}
			}
			delete(e, "MAIAK_PUBLIC_URL")
			if _, err := load(env(e)); err == nil || !strings.Contains(err.Error(), "MAIAK_PUBLIC_URL") {
				t.Errorf("missing MAIAK_PUBLIC_URL accepted: %v", err)
			}
		})
	}
}
