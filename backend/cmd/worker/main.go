// Command worker will claim due checks and run them. In slice 1 it only connects to the
// database and serves the admin endpoints, so the deployment and its probes can be verified.
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
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.LogLevel).With("service", "worker", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL.Value(), db.Options{MaxConns: cfg.DatabaseMaxConns, ApplicationName: "maiak-worker"})
	if err != nil {
		return err
	}
	defer pool.Close()

	reg := metrics.NewRegistry("worker", version, commit)
	log.Info("worker started; no check execution in this slice")
	return httpx.Serve(ctx, log, cfg.ShutdownTimeout,
		httpx.NewServer(cfg.AdminAddr, httpx.AdminHandler(log, pool.Ping, reg), log),
	)
}
