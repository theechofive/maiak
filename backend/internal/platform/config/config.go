// Package config loads and validates process configuration from MAIAK_* environment variables.
// Every loader collects all problems and returns them together, so a misconfigured pod reports
// everything at once. Secret values are wrapped in Secret and never printed.
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// LookupFunc reads an environment variable; os.LookupEnv in production.
type LookupFunc func(string) (string, bool)

// Secret is a string that redacts itself when printed or logged.
type Secret string

// Value returns the secret value. Call sites are easy to grep for.
func (s Secret) Value() string { return string(s) }

func (Secret) String() string { return "[redacted]" }

// GoString redacts %#v as well.
func (Secret) GoString() string { return `"[redacted]"` }

// LogValue redacts the value in slog output.
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// EncryptionKeys maps key id to a 32-byte AES key. It redacts itself when printed.
type EncryptionKeys map[string][]byte

func (EncryptionKeys) String() string { return "[redacted]" }

// GoString redacts %#v as well.
func (EncryptionKeys) GoString() string { return "[redacted]" }

// LogValue redacts the keys in slog output.
func (EncryptionKeys) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Environments accepted in MAIAK_ENV.
var environments = []string{"development", "test", "staging", "production"}

// Base is shared by every process.
type Base struct {
	Env      string
	LogLevel slog.Level
}

// Server is shared by the long-running processes (api, worker, jobs).
type Server struct {
	Base
	AdminAddr        string
	DatabaseURL      Secret
	ShutdownTimeout  time.Duration
	DatabaseMaxConns int32
}

// Encryption holds the keys of the secrets stored in the database. Only api and jobs load it;
// the worker never gets the keys (ADR-0016, L-042).
type Encryption struct {
	EncryptionKeys  EncryptionKeys
	EncryptionKeyID string
}

// Guard is the SSRF guard configuration shared by every process that uses the guarded client.
type Guard struct {
	// BlockedCIDRs are blocked by the SSRF guard in addition to its built-in ranges
	// (MAIAK_BLOCKED_CIDRS, comma-separated, ADR-0016, ADR-0018).
	BlockedCIDRs []netip.Prefix
}

// API is the configuration of cmd/api.
type API struct {
	Server
	Encryption
	Guard
	HTTPAddr string
	// AdminLogin is the admin's login (MAIAK_ADMIN_LOGIN), lowercased (ADR-0009).
	AdminLogin        string
	AdminPasswordHash Secret
	SessionTTL        time.Duration
	CookieSecure      bool
	TrustedProxyHops  int
	// PublicURL is where users open Maiak (MAIAK_PUBLIC_URL, https, no trailing slash); used in
	// Slack messages.
	PublicURL string
}

// Worker is the configuration of cmd/worker. It has no encryption keys (L-042).
type Worker struct {
	Server
	Guard
	// ClaimBatch is the most (monitor, location) jobs one scheduler tick claims
	// (MAIAK_WORKER_CLAIM_BATCH, default 100).
	ClaimBatch int
	// Concurrency caps checks in flight in one worker process (MAIAK_WORKER_CONCURRENCY,
	// default 50); TenantConcurrency and HostConcurrency cap them per tenant and per target
	// host (MAIAK_WORKER_TENANT_CONCURRENCY default 5, MAIAK_WORKER_HOST_CONCURRENCY default 2).
	Concurrency       int
	TenantConcurrency int
	HostConcurrency   int
	// Location is the probe_locations code whose checks this worker runs
	// (MAIAK_WORKER_LOCATION, default home-eu-central-1).
	Location string
}

// Jobs is the configuration of cmd/jobs.
type Jobs struct {
	Server
	Encryption
	Guard
	// PublicURL is MAIAK_PUBLIC_URL, linked from Slack messages.
	PublicURL string
}

// Migrate is the configuration of cmd/migrate.
type Migrate struct {
	Base
	DatabaseURL Secret
}

// Bootstrap is the configuration of `maiakctl db bootstrap`.
type Bootstrap struct {
	Base
	Host          string
	Port          int
	AdminUser     string
	AdminPassword Secret
	DBName        string
	SSLMode       string
	SecretName    string
	// Namespace is empty when MAIAK_BOOTSTRAP_NAMESPACE is unset; the caller then reads the
	// pod's service account namespace.
	Namespace string
}

// shutdownTimeout leaves 5 s of the chart's 30 s termination grace period for the process to exit.
const shutdownTimeout = 25 * time.Second

// LoadAPI loads the api configuration.
func LoadAPI(lookup LookupFunc) (API, error) {
	l := &loader{lookup: lookup}
	cfg := API{
		Server:            l.server(10),
		Encryption:        l.encryption(),
		Guard:             l.guard(),
		HTTPAddr:          l.addr("MAIAK_HTTP_ADDR", ":8080"),
		AdminLogin:        l.login("MAIAK_ADMIN_LOGIN"),
		AdminPasswordHash: l.passwordHash("MAIAK_ADMIN_PASSWORD_HASH"),
		SessionTTL:        l.duration("MAIAK_SESSION_TTL", 12*time.Hour, 5*time.Minute, 30*24*time.Hour),
		CookieSecure:      l.boolean("MAIAK_COOKIE_SECURE", true),
		TrustedProxyHops:  l.integer("MAIAK_TRUSTED_PROXY_HOPS", 1, 0, 10),
		PublicURL:         l.publicURL("MAIAK_PUBLIC_URL"),
	}
	if cfg.HTTPAddr != "" && cfg.HTTPAddr == cfg.AdminAddr {
		l.fail("MAIAK_HTTP_ADDR", "must differ from MAIAK_ADMIN_ADDR")
	}
	return cfg, l.err()
}

// LoadWorker loads the worker configuration.
func LoadWorker(lookup LookupFunc) (Worker, error) {
	l := &loader{lookup: lookup}
	cfg := Worker{
		Server:            l.server(5),
		Guard:             l.guard(),
		ClaimBatch:        l.integer("MAIAK_WORKER_CLAIM_BATCH", 100, 1, 1000),
		Concurrency:       l.integer("MAIAK_WORKER_CONCURRENCY", 50, 1, 1000),
		TenantConcurrency: l.integer("MAIAK_WORKER_TENANT_CONCURRENCY", 5, 1, 1000),
		HostConcurrency:   l.integer("MAIAK_WORKER_HOST_CONCURRENCY", 2, 1, 1000),
		Location:          l.dnsLabel("MAIAK_WORKER_LOCATION", "home-eu-central-1"),
	}
	return cfg, l.err()
}

// LoadJobs loads the jobs configuration.
func LoadJobs(lookup LookupFunc) (Jobs, error) {
	l := &loader{lookup: lookup}
	cfg := Jobs{Server: l.server(5), Encryption: l.encryption(), Guard: l.guard(), PublicURL: l.publicURL("MAIAK_PUBLIC_URL")}
	return cfg, l.err()
}

// LoadMigrate loads the migrate configuration.
func LoadMigrate(lookup LookupFunc) (Migrate, error) {
	l := &loader{lookup: lookup}
	cfg := Migrate{Base: l.base(), DatabaseURL: l.databaseURL("MAIAK_MIGRATE_DATABASE_URL")}
	return cfg, l.err()
}

// LoadBootstrap loads the `maiakctl db bootstrap` configuration.
func LoadBootstrap(lookup LookupFunc) (Bootstrap, error) {
	l := &loader{lookup: lookup}
	cfg := Bootstrap{
		Base:          l.base(),
		Host:          l.required("MAIAK_BOOTSTRAP_HOST"),
		Port:          l.integer("MAIAK_BOOTSTRAP_PORT", 5432, 1, 65535),
		AdminUser:     l.required("MAIAK_BOOTSTRAP_ADMIN_USER"),
		AdminPassword: Secret(l.required("MAIAK_BOOTSTRAP_ADMIN_PASSWORD")),
		DBName:        l.identifier("MAIAK_BOOTSTRAP_DB_NAME", "maiak"),
		SSLMode:       l.oneOf("MAIAK_BOOTSTRAP_SSLMODE", "require", "disable", "allow", "prefer", "require", "verify-ca", "verify-full"),
		SecretName:    l.dnsLabel("MAIAK_BOOTSTRAP_SECRET_NAME", "maiak-db"),
		Namespace:     l.optional("MAIAK_BOOTSTRAP_NAMESPACE"),
	}
	if cfg.Namespace != "" && !isDNSLabel(cfg.Namespace) {
		l.fail("MAIAK_BOOTSTRAP_NAMESPACE", "must be a DNS label")
	}
	return cfg, l.err()
}

type loader struct {
	lookup LookupFunc
	errs   []error
}

func (l *loader) fail(name, msg string) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", name, msg))
}

func (l *loader) err() error {
	if len(l.errs) == 0 {
		return nil
	}
	msgs := make([]string, len(l.errs))
	for i, e := range l.errs {
		msgs[i] = e.Error()
	}
	return errors.New("invalid configuration:\n  " + strings.Join(msgs, "\n  "))
}

func (l *loader) optional(name string) string {
	v, _ := l.lookup(name)
	return strings.TrimSpace(v)
}

func (l *loader) str(name, def string) string {
	if v := l.optional(name); v != "" {
		return v
	}
	return def
}

func (l *loader) required(name string) string {
	v := l.optional(name)
	if v == "" {
		l.fail(name, "is required")
	}
	return v
}

func (l *loader) base() Base {
	b := Base{Env: l.oneOf("MAIAK_ENV", "production", environments...)}
	level, err := logging.ParseLevel(l.str("MAIAK_LOG_LEVEL", "info"))
	if err != nil {
		l.fail("MAIAK_LOG_LEVEL", err.Error())
	}
	b.LogLevel = level
	return b
}

func (l *loader) server(defaultMaxConns int32) Server {
	s := Server{
		Base:             l.base(),
		AdminAddr:        l.addr("MAIAK_ADMIN_ADDR", ":9090"),
		DatabaseURL:      l.databaseURL("MAIAK_DATABASE_URL"),
		ShutdownTimeout:  shutdownTimeout,
		DatabaseMaxConns: defaultMaxConns,
	}
	return s
}

func (l *loader) encryption() Encryption {
	var e Encryption
	e.EncryptionKeys, e.EncryptionKeyID = l.encryptionKeys()
	return e
}

func (l *loader) guard() Guard {
	return Guard{BlockedCIDRs: l.cidrs("MAIAK_BLOCKED_CIDRS")}
}

func (l *loader) addr(name, def string) string {
	v := l.str(name, def)
	if _, port, err := net.SplitHostPort(v); err != nil || port == "" {
		l.fail(name, "must be host:port or :port")
	}
	return v
}

func (l *loader) databaseURL(name string) Secret {
	v := l.required(name)
	if v == "" {
		return ""
	}
	// Do not include the parse error: it can echo parts of the URL.
	if _, err := pgx.ParseConfig(v); err != nil {
		l.fail(name, "is not a valid PostgreSQL connection string")
	}
	return Secret(v)
}

// publicURL accepts an absolute https URL without user info, query, fragment or trailing slash,
// made only of characters that need no escaping in a Slack link (<url|text>).
func (l *loader) publicURL(name string) string {
	v := l.required(name)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || strings.HasSuffix(v, "/") || strings.ContainsAny(v, "|<>& \t\n\"'") || u.String() != v {
		l.fail(name, "must be an https URL without a trailing slash, such as https://maiak.example.com")
	}
	return v
}

func (l *loader) login(name string) string {
	v := l.required(name)
	if v == "" {
		return ""
	}
	n, err := NormalizeLogin(v)
	if err != nil {
		l.fail(name, err.Error())
	}
	return n
}

// NormalizeLogin lowercases a login and checks that it is 1-64 characters of [a-z0-9._@-]
// (ADR-0009). Logins are case-insensitive: every input is normalised with this function.
func NormalizeLogin(s string) (string, error) {
	if len(s) == 0 || len(s) > 64 {
		return "", fmt.Errorf("login must be 1-64 characters, got %d", len(s))
	}
	// Checked before lowercasing: strings.ToLower maps some non-ASCII runes (the Kelvin sign) to ASCII.
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._@-", c) {
			return "", errors.New("login must contain only letters, digits and . _ @ -")
		}
	}
	return strings.ToLower(s), nil
}

func (l *loader) passwordHash(name string) Secret {
	v := l.required(name)
	if v == "" {
		return ""
	}
	if _, err := crypto.ParseArgon2idHash(v); err != nil {
		l.fail(name, "must be an argon2id PHC hash (generate with `maiakctl hashpw`)")
	}
	return Secret(v)
}

func (l *loader) duration(name string, def, minimum, maximum time.Duration) time.Duration {
	v := l.optional(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(name, "must be a Go duration such as 12h")
		return def
	}
	if d < minimum || d > maximum {
		l.fail(name, fmt.Sprintf("must be between %s and %s", minimum, maximum))
	}
	return d
}

func (l *loader) boolean(name string, def bool) bool {
	v := l.optional(name)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(name, "must be true or false")
		return def
	}
	return b
}

func (l *loader) integer(name string, def, minimum, maximum int) int {
	v := l.optional(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minimum || n > maximum {
		l.fail(name, fmt.Sprintf("must be an integer between %d and %d", minimum, maximum))
		return def
	}
	return n
}

func (l *loader) cidrs(name string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(l.optional(name), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			l.fail(name, fmt.Sprintf("%q is not a CIDR such as 10.0.0.0/8", part))
			continue
		}
		out = append(out, p.Masked())
	}
	return out
}

func (l *loader) oneOf(name, def string, allowed ...string) string {
	v := l.str(name, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.fail(name, "must be one of "+strings.Join(allowed, ", "))
	return v
}

// identifier accepts lower-case SQL identifiers (database names are interpolated into DDL,
// quoted, but a narrow alphabet keeps them boring).
func (l *loader) identifier(name, def string) string {
	v := l.str(name, def)
	if !isIdentifier(v) {
		l.fail(name, "must match [a-z_][a-z0-9_]{0,62}")
	}
	return v
}

func (l *loader) dnsLabel(name, def string) string {
	v := l.str(name, def)
	if !isDNSLabel(v) {
		l.fail(name, "must be a DNS label")
	}
	return v
}

func (l *loader) encryptionKeys() (EncryptionKeys, string) {
	raw := l.required("MAIAK_ENCRYPTION_KEYS")
	active := l.required("MAIAK_ENCRYPTION_ACTIVE_KEY_ID")
	if raw == "" {
		return nil, active
	}
	var encoded map[string]string
	if err := json.Unmarshal([]byte(raw), &encoded); err != nil {
		l.fail("MAIAK_ENCRYPTION_KEYS", `must be a JSON object {"<keyId>":"<base64 32 bytes>"}`)
		return nil, active
	}
	keys := make(EncryptionKeys, len(encoded))
	for id, b64 := range encoded {
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(key) != 32 {
			l.fail("MAIAK_ENCRYPTION_KEYS", fmt.Sprintf("key %q must be standard base64 of 32 bytes", id))
			continue
		}
		keys[id] = key
	}
	if active != "" && len(keys) == len(encoded) {
		// NewKeyring checks key ids and that the active key exists.
		if _, err := crypto.NewKeyring(keys, active); err != nil {
			l.fail("MAIAK_ENCRYPTION_KEYS", err.Error())
		}
	}
	return keys, active
}

func isIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_' || (c >= 'a' && c <= 'z'):
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func isDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
