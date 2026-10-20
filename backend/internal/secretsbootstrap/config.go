package secretsbootstrap

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// Modes for each Secret.
const (
	ModeGenerate = "generate"
	ModeSkip     = "skip"
)

// DefaultLogin is the system admin login (like Grafana's `admin`).
const DefaultLogin = "admin"

// Config is the environment contract of `maiakctl secrets bootstrap`.
type Config struct {
	LogLevel slog.Level
	// Namespace is empty when MAIAK_SECRETS_NAMESPACE is unset; the caller then reads the pod's
	// service account namespace.
	Namespace string
	Options   Options
}

// LoadConfig reads MAIAK_SECRETS_* (and MAIAK_LOG_LEVEL) and reports every problem at once.
// It lives here rather than in platform/config because only this command uses it.
func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	var errs []string
	fail := func(name, msg string) { errs = append(errs, name+": "+msg) }
	get := func(name, def string) string {
		v, _ := lookup(name)
		if v = strings.TrimSpace(v); v == "" {
			return def
		}
		return v
	}
	label := func(name, def string) string {
		v := get(name, def)
		if !isDNSLabel(v) {
			fail(name, "must be a DNS label")
		}
		return v
	}
	mode := func(name string) string {
		v := get(name, ModeGenerate)
		if v != ModeGenerate && v != ModeSkip {
			fail(name, "must be "+ModeGenerate+" or "+ModeSkip)
		}
		return v
	}

	var c Config
	level, err := logging.ParseLevel(get("MAIAK_LOG_LEVEL", "info"))
	if err != nil {
		fail("MAIAK_LOG_LEVEL", err.Error())
	}
	c.LogLevel = level
	c.Namespace = get("MAIAK_SECRETS_NAMESPACE", "")
	if c.Namespace != "" && !isDNSLabel(c.Namespace) {
		fail("MAIAK_SECRETS_NAMESPACE", "must be a DNS label")
	}
	o := Options{
		AdminMode:              mode("MAIAK_SECRETS_ADMIN_MODE"),
		CryptoMode:             mode("MAIAK_SECRETS_CRYPTO_MODE"),
		AdminLogin:             get("MAIAK_SECRETS_ADMIN_LOGIN", DefaultLogin),
		AdminSecretName:        label("MAIAK_SECRETS_ADMIN_SECRET_NAME", "maiak-admin"),
		AdminInitialSecretName: label("MAIAK_SECRETS_ADMIN_INITIAL_SECRET_NAME", "maiak-admin-initial"),
		CryptoSecretName:       label("MAIAK_SECRETS_CRYPTO_SECRET_NAME", "maiak-crypto"),
		CryptoKeyID:            get("MAIAK_SECRETS_CRYPTO_KEY_ID", "k1"),
	}
	if o.AdminMode == ModeGenerate {
		login, err := config.NormalizeLogin(o.AdminLogin)
		if err != nil {
			fail("MAIAK_SECRETS_ADMIN_LOGIN", err.Error())
		}
		o.AdminLogin = login
	}
	if o.AdminSecretName == o.AdminInitialSecretName {
		fail("MAIAK_SECRETS_ADMIN_INITIAL_SECRET_NAME", "must differ from MAIAK_SECRETS_ADMIN_SECRET_NAME")
	}
	if err := crypto.ValidateKeyID(o.CryptoKeyID); err != nil {
		fail("MAIAK_SECRETS_CRYPTO_KEY_ID", err.Error())
	}
	c.Options = o
	if len(errs) > 0 {
		return c, errors.New("invalid configuration:\n  " + strings.Join(errs, "\n  "))
	}
	return c, nil
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
