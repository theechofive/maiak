// Package jobs is cmd/jobs: it delivers the notification outbox (claim_deliveries /
// finish_delivery) through the Slack notifier, and runs the singleton housekeeping tasks
// (partitions, rollups, retention, session purge) under Postgres advisory locks (ADR-0051).
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// Notifier sends one message to a webhook; *notify.SlackNotifier implements it. Failures must
// be *notify.SendError (anything else is treated as a retryable "other").
type Notifier interface {
	Send(ctx context.Context, webhookURL, text string) error
}

// Defaults of DeliveryConfig.
const (
	DefaultDeliveryPoll        = time.Second
	DefaultDeliveryBatch       = 20
	DefaultDeliveryConcurrency = 10
	DefaultChannelInterval     = time.Second
	DefaultDrainTimeout        = 25 * time.Second
	DefaultGaugeInterval       = 15 * time.Second
	// dbTimeout bounds one claim or finish transaction.
	dbTimeout = 10 * time.Second
)

// DeliveryConfig tunes the Deliverer; zero values take the defaults.
type DeliveryConfig struct {
	PollInterval time.Duration
	Batch        int
	// Concurrency caps deliveries in flight in this process. It also bounds how long a delivery
	// waits for its channel's rate limit (at most Concurrency x ChannelInterval).
	Concurrency     int
	ChannelInterval time.Duration
	DrainTimeout    time.Duration
	GaugeInterval   time.Duration
	// PublicURL is MAIAK_PUBLIC_URL, linked from the messages.
	PublicURL string
}

// DeliveryDeps are the Deliverer's collaborators.
type DeliveryDeps struct {
	Pool     *db.Pool
	Channels *notify.Service
	Notifier Notifier
	Metrics  *Metrics
	Log      *slog.Logger
	Clock    clock.Clock
	// Sleep waits d or until ctx ends; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Deliverer delivers the outbox.
type Deliverer struct {
	cfg      DeliveryConfig
	d        DeliveryDeps
	limiter  *channelLimiter
	slots    chan struct{}
	inflight sync.WaitGroup
}

// delivery is one claimed outbox row.
type delivery struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Kind      string
	MonitorID uuid.UUID
	ChannelID uuid.UUID
	Payload   []byte
	Attempts  int
	// Token fences finish_delivery: only the holder of this claim can record its outcome.
	Token uuid.UUID
}

// NewDeliverer returns a Deliverer.
func NewDeliverer(cfg DeliveryConfig, d DeliveryDeps) *Deliverer {
	cfg.PollInterval = orDefault(cfg.PollInterval, DefaultDeliveryPoll)
	cfg.Batch = orDefault(cfg.Batch, DefaultDeliveryBatch)
	cfg.Concurrency = orDefault(cfg.Concurrency, DefaultDeliveryConcurrency)
	cfg.ChannelInterval = orDefault(cfg.ChannelInterval, DefaultChannelInterval)
	cfg.DrainTimeout = orDefault(cfg.DrainTimeout, DefaultDrainTimeout)
	cfg.GaugeInterval = orDefault(cfg.GaugeInterval, DefaultGaugeInterval)
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.Sleep == nil {
		d.Sleep = sleep
	}
	return &Deliverer{cfg: cfg, d: d, limiter: newChannelLimiter(cfg.ChannelInterval), slots: make(chan struct{}, cfg.Concurrency)}
}

// Run claims and delivers every PollInterval until ctx ends, then stops claiming and waits up
// to DrainTimeout for sends in flight. A send still running after that is cancelled; its lease
// (2 minutes) expires and the row is delivered again (at-least-once, ADR-0007).
func (r *Deliverer) Run(ctx context.Context) error {
	base := context.WithoutCancel(ctx)
	sendCtx, abandon := context.WithCancel(base)
	defer abandon()

	r.d.Log.Info("delivery loop started", "concurrency", r.cfg.Concurrency)
	tick := time.NewTicker(r.cfg.PollInterval)
	defer tick.Stop()
	gauge := time.NewTicker(r.cfg.GaugeInterval)
	defer gauge.Stop()
	r.refreshPending(ctx)
	for {
		r.tick(ctx, sendCtx)
		select {
		case <-ctx.Done():
		case <-gauge.C:
			r.refreshPending(ctx)
			continue
		case <-tick.C:
			continue
		}
		break
	}

	r.d.Log.Info("delivery loop stopping: waiting for sends in flight", "timeout", r.cfg.DrainTimeout)
	done := make(chan struct{})
	go func() { r.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(r.cfg.DrainTimeout):
		r.d.Log.Warn("abandoning deliveries in flight; their leases will expire")
		abandon()
		<-done
	}
	r.d.Log.Info("delivery loop stopped")
	return nil
}

// RunOnce claims once and waits for those deliveries to finish (tests and tools).
func (r *Deliverer) RunOnce(ctx context.Context) int {
	n := r.tick(ctx, ctx)
	r.inflight.Wait()
	return n
}

// tick claims as many rows as there are free slots and starts one goroutine per row.
func (r *Deliverer) tick(ctx, sendCtx context.Context) int {
	free := cap(r.slots) - len(r.slots)
	if free <= 0 || ctx.Err() != nil {
		return 0
	}
	claimed, err := r.claim(ctx, min(free, r.cfg.Batch))
	if err != nil {
		r.d.Metrics.claimErrors.Inc()
		r.d.Log.ErrorContext(ctx, "claim_deliveries failed", "error", err)
		return 0
	}
	for _, dl := range claimed {
		r.slots <- struct{}{}
		r.inflight.Add(1)
		go func() {
			defer func() { <-r.slots; r.inflight.Done() }()
			r.deliver(sendCtx, dl)
		}()
	}
	return len(claimed)
}

func (r *Deliverer) claim(ctx context.Context, n int) ([]delivery, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var out []delivery
	err := db.WithTx(ctx, r.d.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, kind, monitor_id, channel_id, payload, attempts, claim_token FROM claim_deliveries($1)`, n)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (delivery, error) {
			var dl delivery
			var attempts int32
			err := row.Scan(&dl.ID, &dl.TenantID, &dl.Kind, &dl.MonitorID, &dl.ChannelID, &dl.Payload, &attempts, &dl.Token)
			dl.Attempts = int(attempts)
			return dl, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: claim deliveries: %w", err)
	}
	return out, nil
}

// deliver resolves the channel, sends and records the outcome. Nothing it logs or stores
// contains the webhook URL or a raw transport error.
func (r *Deliverer) deliver(ctx context.Context, dl delivery) {
	ctx, _ = logging.WithFields(ctx)
	log := r.d.Log.With("delivery_id", dl.ID, "monitor_id", dl.MonitorID, "kind", dl.Kind)

	var target notify.Target
	err := r.tenantTx(ctx, dl.TenantID, func(tx pgx.Tx) error {
		var err error
		target, err = r.d.Channels.DeliveryTarget(ctx, tx, dl.TenantID, dl.ChannelID)
		return err
	})
	var reason string
	switch {
	case errors.Is(err, notify.ErrChannelNotFound):
		reason = "channel_missing"
	case errors.Is(err, notify.ErrUndecryptable):
		reason = "channel_undecryptable"
	case err != nil:
		log.ErrorContext(ctx, "reading the delivery channel failed; the lease will expire", "error", err)
		return
	case target.Status != notify.StatusActive:
		reason = "channel_failed"
	}
	if reason != "" {
		r.finish(ctx, log, dl, outcome{status: statusFailed, errKind: reason})
		return
	}
	text, err := notify.Format(dl.Kind, dl.Payload, r.cfg.PublicURL)
	if err != nil {
		r.finish(ctx, log, dl, outcome{status: statusFailed, errKind: "bad_payload"})
		return
	}

	if wait := r.limiter.reserve(dl.ChannelID, r.d.Clock.Now()); wait > 0 {
		if err := r.d.Sleep(ctx, wait); err != nil {
			return // abandoned at shutdown; the lease expires
		}
	}
	start := time.Now()
	sendErr := r.d.Notifier.Send(ctx, target.WebhookURL, text)
	r.d.Metrics.duration.Observe(time.Since(start).Seconds())
	if sendErr != nil && ctx.Err() != nil {
		return // abandoned at shutdown: not a real failure
	}
	r.finish(ctx, log, dl, decide(dl.Attempts, sendErr, r.d.Clock.Now()))
}

// finish records the outcome with finish_delivery and, for a revoked webhook, flags the channel,
// in one tenant transaction. It runs even if the send context was cancelled meanwhile.
func (r *Deliverer) finish(ctx context.Context, log *slog.Logger, dl delivery, o outcome) {
	ctx = context.WithoutCancel(ctx)
	var errText *string
	if o.errKind != "" {
		errText = &o.errKind
	}
	var retryAt *time.Time
	if o.status == statusPending {
		retryAt = &o.retryAt
	}
	var recorded bool
	err := r.tenantTx(ctx, dl.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT finish_delivery($1, $2, $3, $4, $5)`, dl.ID, dl.Token, o.status, errText, retryAt).Scan(&recorded); err != nil {
			return err
		}
		if recorded && o.failChannel {
			changed, err := r.d.Channels.MarkFailed(ctx, tx, dl.TenantID, dl.ChannelID)
			if changed {
				log.WarnContext(ctx, "slack webhook revoked; channel marked failed", "channel_id", dl.ChannelID)
			}
			return err
		}
		return nil
	})
	if err != nil {
		r.d.Log.ErrorContext(ctx, "finish_delivery failed; the lease will expire", "delivery_id", dl.ID, "error", err)
		return
	}
	if !recorded {
		log.WarnContext(ctx, "delivery claim was lost before finishing (lease expired or claimed again)")
		return
	}
	label := o.status
	if label == statusPending {
		label = "retry"
	}
	r.d.Metrics.deliveries.WithLabelValues(label).Inc()
	if o.status != statusSent {
		log.WarnContext(ctx, "delivery not sent", "status", o.status, "error_kind", o.errKind, "attempt", dl.Attempts+1)
	}
}

func (r *Deliverer) tenantTx(ctx context.Context, tenant uuid.UUID, fn db.TxFunc) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return db.WithTenantTx(ctx, r.d.Pool, tenant, fn)
}

func (r *Deliverer) refreshPending(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var n int64
	err := db.WithTx(ctx, r.d.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT outbox_pending()`).Scan(&n)
	})
	if err != nil {
		if ctx.Err() == nil {
			r.d.Log.WarnContext(ctx, "outbox_pending failed", "error", err)
		}
		return
	}
	r.d.Metrics.pending.Set(float64(n))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
