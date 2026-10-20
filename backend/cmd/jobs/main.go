// Command jobs delivers the notification outbox to Slack and runs the singleton housekeeping
// tasks: partitions, hourly rollups, retention and expired-session purge (internal/jobs).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/theechofive/maiak/backend/internal/jobs"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
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
	cfg, err := config.LoadJobs(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.LogLevel).With("service", "jobs", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	keys, err := crypto.NewKeyring(cfg.EncryptionKeys, cfg.EncryptionKeyID)
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL.Value(), db.Options{MaxConns: cfg.DatabaseMaxConns, ApplicationName: "maiak-jobs"})
	if err != nil {
		return err
	}
	defer pool.Close()

	reg := metrics.NewRegistry("jobs", version, commit)
	m := jobs.NewMetrics(reg)
	deliverer := jobs.NewDeliverer(jobs.DeliveryConfig{PublicURL: cfg.PublicURL, DrainTimeout: cfg.ShutdownTimeout}, jobs.DeliveryDeps{
		Pool:     pool,
		Channels: notify.NewService(pool, keys, nil, cfg.PublicURL),
		Notifier: notify.NewSlackNotifier(notify.NewSlackClient(cfg.BlockedCIDRs)),
		Metrics:  m,
		Log:      log,
	})
	housekeeper := jobs.NewHousekeeper(pool, jobs.DefaultTasks(), m, log, clock.System{})

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return httpx.Serve(gctx, log, cfg.ShutdownTimeout,
			httpx.NewServer(cfg.AdminAddr, httpx.AdminHandler(log, pool.Ping, reg), log))
	})
	g.Go(func() error { return deliverer.Run(gctx) })
	g.Go(func() error { return housekeeper.Run(gctx) })
	return g.Wait()
}
