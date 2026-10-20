// Command api serves the REST API (api/openapi.yaml) and the admin endpoints.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
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

	reg := metrics.NewRegistry("api", version, commit)
	authSvc, err := auth.NewService(pool, auth.Options{
		SessionTTL: cfg.SessionTTL,
		Clock:      clock.System{},
		Logger:     log,
		Registerer: reg,
	})
	if err != nil {
		return err
	}
	if err := authSvc.BootstrapAdmin(ctx, cfg.AdminEmail, cfg.AdminPasswordHash.Value()); err != nil {
		return err
	}
	log.Info("admin bootstrap done")

	handler, err := api.NewHandler(api.Config{
		Version:          version,
		Commit:           commit,
		CookieSecure:     cfg.CookieSecure,
		TrustedProxyHops: cfg.TrustedProxyHops,
	}, api.Deps{Auth: authSvc, Logger: log, Metrics: metrics.NewHTTP(reg)})
	if err != nil {
		return err
	}

	return httpx.Serve(ctx, log, cfg.ShutdownTimeout,
		httpx.NewServer(cfg.HTTPAddr, handler, log),
		httpx.NewServer(cfg.AdminAddr, httpx.AdminHandler(log, pool.Ping, reg), log),
	)
}
