// Command api serves the REST API (api/openapi.yaml) and the admin endpoints.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/theechofive/maiak/backend/internal/admin"
	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/entitlements"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/oauth"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/projects"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/statuspages"
	"github.com/theechofive/maiak/backend/migrations"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.LogLevel).With("service", "api", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL.Value(), db.Options{MaxConns: cfg.DatabaseMaxConns, ApplicationName: "maiak-api"})
	if err != nil {
		return err
	}
	defer pool.Close()
	// An image older than the schema must not run against it (O-06, L-282, L-311).
	if err := migrations.CheckNotAhead(ctx, pool); err != nil {
		return err
	}

	reg := metrics.NewRegistry("api", version, commit)
	authSvc, err := auth.NewService(pool, auth.Options{
		SessionTTL: cfg.SessionTTL,
		Clock:      clock.System{},
		Logger:     log,
		Registerer: reg,
		// ADR-0104: only the admin login keeps the password form when it is off.
		PasswordLoginDisabled: !cfg.PasswordLoginEnabled,
		// O-04: only operator-approved Workspace domains can be claimed and joined by.
		AllowedDomains: cfg.OAuth.AllowedDomains,
	})
	if err != nil {
		return err
	}
	if err := authSvc.BootstrapAdmin(ctx, cfg.AdminLogin, cfg.AdminPasswordHash.Value()); err != nil {
		return err
	}
	log.Info("admin bootstrap done")

	adminSvc, err := admin.NewService(pool, authSvc, admin.Options{Logger: log, Registerer: reg})
	if err != nil {
		return err
	}

	keys, err := crypto.NewKeyring(cfg.EncryptionKeys, cfg.EncryptionKeyID)
	if err != nil {
		return err
	}
	flow, err := oauthFlow(cfg)
	if err != nil {
		return err
	}
	if flow != nil {
		names := make([]string, 0, len(flow.Providers()))
		for _, p := range flow.Providers() {
			names = append(names, p.Name())
		}
		log.Info("oauth sign-in enabled", "providers", names, "password_login_enabled", cfg.PasswordLoginEnabled)
	}

	slack := notify.NewSlackNotifier(notify.NewSlackClient(cfg.BlockedCIDRs))

	sched := scheduler.New()
	handler, err := api.NewHandler(api.Config{
		Version:              version,
		Commit:               commit,
		CookieSecure:         cfg.CookieSecure,
		TrustedProxyHops:     cfg.TrustedProxyHops,
		RegistrationEnabled:  cfg.RegistrationEnabled,
		PublicURL:            cfg.PublicURL,
		PasswordLoginEnabled: cfg.PasswordLoginEnabled,
	}, api.Deps{
		Auth:     authSvc,
		Monitors: monitors.NewService(pool, sched, incidents.NewService(sched)),
		Queries:  queries.New(pool, nil),
		Slack:    notify.NewService(pool, keys, slack, cfg.PublicURL),
		Plans:    plans.NewService(pool), Projects: projects.NewService(pool),
		StatusPages:  statuspages.NewService(pool, statuspages.Options{Logger: log}),
		Admin:        adminSvc,
		OAuth:        flow,
		Entitlements: entitlements.NewService(pool),
		Logger:       log,
		Metrics:      metrics.NewHTTP(reg),
		Pings:        api.NewPingMetrics(reg),
	})
	if err != nil {
		return err
	}

	return httpx.Serve(ctx, log, cfg.ShutdownTimeout,
		httpx.NewServer(cfg.HTTPAddr, handler, log),
		httpx.NewServer(cfg.AdminAddr, httpx.AdminHandler(log, pool.Ping, reg), log),
	)
}

// oauthFlow builds the OAuth providers that have credentials (ADR-0104); nil when there are none.
// The state cookie's key is derived from the active encryption key, so every replica shares it.
func oauthFlow(cfg config.API) (*oauth.Flow, error) {
	var providers []oauth.Provider
	if cfg.OAuth.GoogleEnabled() {
		providers = append(providers, oauth.NewGoogle(oauth.GoogleConfig{
			ClientID: cfg.OAuth.GoogleClientID, ClientSecret: cfg.OAuth.GoogleClientSecret.Value(),
		}))
	}
	if cfg.OAuth.GitHubEnabled() {
		providers = append(providers, oauth.NewGitHub(oauth.GitHubConfig{
			ClientID: cfg.OAuth.GitHubClientID, ClientSecret: cfg.OAuth.GitHubClientSecret.Value(),
		}))
	}
	if len(providers) == 0 {
		return nil, nil
	}
	key, err := oauth.DeriveKey(cfg.EncryptionKeys[cfg.EncryptionKeyID])
	if err != nil {
		return nil, fmt.Errorf("oauth state key: %w", err)
	}
	return oauth.NewFlow(key, cfg.PublicURL, clock.System{}, providers...)
}
