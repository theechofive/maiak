// Command worker claims due checks from the database, runs them through the SSRF-guarded HTTPS
// prober and records results, incident transitions and outbox rows (internal/worker).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/theechofive/maiak/backend/internal/checks"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/safehttp"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/worker"
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
	sched := scheduler.New()
	client := safehttp.NewClient(safehttp.Options{
		Timeout: checks.DefaultTimeout, MaxRedirects: 3, BlockedPrefixes: cfg.BlockedCIDRs,
	})
	w := worker.New(worker.Config{
		ClaimBatch: cfg.ClaimBatch, Concurrency: cfg.Concurrency,
		TenantConcurrency: cfg.TenantConcurrency, HostConcurrency: cfg.HostConcurrency,
		DrainTimeout: cfg.ShutdownTimeout,
	}, worker.Deps{
		Pool: pool, Scheduler: sched, Incidents: incidents.NewService(sched),
		Prober: checks.NewHTTPSProber(client), Metrics: worker.NewMetrics(reg), Log: log,
	})

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return httpx.Serve(gctx, log, cfg.ShutdownTimeout,
			httpx.NewServer(cfg.AdminAddr, httpx.AdminHandler(log, pool.Ping, reg), log))
	})
	g.Go(func() error { return w.Run(gctx) })
	return g.Wait()
}
