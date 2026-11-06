// Command statuspage serves the public status pages under /status/<slug> (ADR-0103): HTML, JSON,
// RSS, a badge and an embed script, from snapshots prepared by the api and jobs. It runs as the
// database role maiak_statuspage, which may only call status_page_snapshot(slug), and keeps no
// sessions.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/statuspages/public"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "statuspage:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadStatuspage(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.LogLevel).With("service", "statuspage", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL.Value(), db.Options{MaxConns: cfg.DatabaseMaxConns, ApplicationName: "maiak-statuspage"})
	if err != nil {
		return err
	}
	defer pool.Close()

	reg := metrics.NewRegistry("statuspage", version, commit)
	handler := public.NewHandler(public.Config{
		PublicURL:        cfg.PublicURL,
		TrustedProxyHops: cfg.TrustedProxyHops,
		CookieSecure:     cfg.CookieSecure,
	}, public.Deps{
		Source:      public.DBSource{Pool: pool},
		Cookies:     public.NewCookieSigner([]byte(cfg.CookieKey.Value())),
		Clock:       clock.System{},
		Logger:      log,
		Registerer:  reg,
		HTTPMetrics: metrics.NewHTTP(reg),
	})
	return httpx.Serve(ctx, log, cfg.ShutdownTimeout,
		httpx.NewServer(cfg.HTTPAddr, handler, log),
		httpx.NewServer(cfg.AdminAddr, httpx.AdminHandler(log, pool.Ping, reg), log),
	)
}
