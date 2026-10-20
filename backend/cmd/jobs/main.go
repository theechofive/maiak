// Command jobs will run housekeeping (partitions, rollups, retention) and deliver
// notifications from the outbox. In slice 1 it only connects to the database and serves the
// admin endpoints.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jobs:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.LogLevel).With("service", "jobs", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL.Value(), db.Options{MaxConns: cfg.DatabaseMaxConns, ApplicationName: "maiak-jobs"})
	if err != nil {
		return err
	}
	defer pool.Close()

	reg := metrics.NewRegistry("jobs", version, commit)
	log.Info("jobs started; no scheduled tasks in this slice")
	return httpx.Serve(ctx, log, cfg.ShutdownTimeout,
		httpx.NewServer(cfg.AdminAddr, httpx.AdminHandler(log, pool.Ping, reg), log),
	)
}
