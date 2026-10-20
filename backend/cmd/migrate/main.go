// Command migrate applies the embedded database migrations as maiak_owner. It runs as a
// one-shot Kubernetes Job before the workloads are updated.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/migrations"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadMigrate(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.LogLevel).With("service", "migrate", "version", version, "commit", commit)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	connCfg, err := pgx.ParseConfig(cfg.DatabaseURL.Value())
	if err != nil {
		return errors.New("invalid MAIAK_MIGRATE_DATABASE_URL")
	}
	connCfg.RuntimeParams["application_name"] = "maiak-migrate"
	db := stdlib.OpenDB(*connCfg)
	defer db.Close()
	// One connection for the advisory lock, one for the migrations; maiak_owner has CONNECTION LIMIT 3.
	db.SetMaxOpenConns(2)

	results, err := migrations.Up(ctx, db)
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
	}
	if err != nil {
		return err
	}
	if len(results) == 0 {
		log.Info("database is up to date")
	}
	return nil
}
