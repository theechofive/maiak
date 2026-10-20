package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/db"
)

// Task is a singleton housekeeping task. Run executes inside a transaction that holds the task's
// advisory lock, so at most one jobs replica runs it at a time; every task is idempotent, so a
// replica running it again later is harmless.
type Task struct {
	Name  string
	Every time.Duration
	// LockKey is the pg_try_advisory_xact_lock key; unique per task.
	LockKey int64
	Run     func(ctx context.Context, tx pgx.Tx, now time.Time) (int, error)
}

// Advisory lock keys of the tasks. Constants in their own range, so they cannot collide with
// the hashtext() keys the definer functions take themselves.
const (
	lockEnsurePartitions int64 = 0x6d6a_0001
	lockComputeRollups   int64 = 0x6d6a_0002
	lockDropPartitions   int64 = 0x6d6a_0003
	lockPurgeSessions    int64 = 0x6d6a_0004
)

// RollupLookback is how many closed hours compute_rollups recomputes on every run, so late
// results and a missed run are absorbed (the function is an idempotent upsert).
const RollupLookback = 3 * time.Hour

// DefaultTasks are the housekeeping tasks of ADR-0016/ADR-0021.
func DefaultTasks() []Task {
	return []Task{
		{Name: "ensure_partitions", Every: 24 * time.Hour, LockKey: lockEnsurePartitions, Run: scalar(`SELECT ensure_partitions()`)},
		{Name: "compute_rollups", Every: 5 * time.Minute, LockKey: lockComputeRollups, Run: computeRollups},
		{Name: "drop_expired_partitions", Every: 24 * time.Hour, LockKey: lockDropPartitions, Run: scalar(`SELECT drop_expired_partitions()`)},
		{Name: "purge_expired_sessions", Every: time.Hour, LockKey: lockPurgeSessions, Run: scalar(`SELECT purge_expired_sessions()`)},
	}
}

func scalar(sql string) func(context.Context, pgx.Tx, time.Time) (int, error) {
	return func(ctx context.Context, tx pgx.Tx, _ time.Time) (int, error) {
		var n int32
		err := tx.QueryRow(ctx, sql).Scan(&n)
		return int(n), err
	}
}

// computeRollups recomputes the closed hours of the lookback window; the function itself never
// rolls up the open hour.
func computeRollups(ctx context.Context, tx pgx.Tx, now time.Time) (int, error) {
	to := now.UTC().Truncate(time.Hour)
	var n int32
	err := tx.QueryRow(ctx, `SELECT compute_rollups($1, $2)`, to.Add(-RollupLookback), to).Scan(&n)
	return int(n), err
}

// Housekeeper runs the tasks: each once at start, then every Task.Every.
type Housekeeper struct {
	pool    *db.Pool
	tasks   []Task
	metrics *Metrics
	log     *slog.Logger
	clock   clock.Clock
	// Poll is how often due tasks are checked (default 1 s).
	Poll time.Duration
	// Timeout bounds one task run (default 5 min).
	Timeout time.Duration
}

// NewHousekeeper returns a Housekeeper for tasks.
func NewHousekeeper(pool *db.Pool, tasks []Task, m *Metrics, log *slog.Logger, clk clock.Clock) *Housekeeper {
	if clk == nil {
		clk = clock.System{}
	}
	for _, t := range tasks {
		m.housekeeping.WithLabelValues(t.Name, resultOK)
		m.housekeeping.WithLabelValues(t.Name, resultSkipped)
		m.housekeeping.WithLabelValues(t.Name, resultError)
	}
	return &Housekeeper{pool: pool, tasks: tasks, metrics: m, log: log, clock: clk, Poll: time.Second, Timeout: 5 * time.Minute}
}

// Results of one task run (metric label).
const (
	resultOK      = "ok"
	resultSkipped = "skipped"
	resultError   = "error"
)

// Run executes due tasks until ctx ends. A task running at shutdown is cancelled and rolls back.
func (h *Housekeeper) Run(ctx context.Context) error {
	next := make([]time.Time, len(h.tasks))
	tick := time.NewTicker(h.Poll)
	defer tick.Stop()
	for {
		for i, t := range h.tasks {
			now := h.clock.Now()
			if ctx.Err() != nil || now.Before(next[i]) {
				continue
			}
			// A skipped or failed run is retried at the next interval, except that a failure
			// retries sooner (1 minute) so a transient error does not cost a whole day.
			switch h.RunTask(ctx, t) {
			case resultError:
				next[i] = now.Add(min(time.Minute, t.Every))
			default:
				next[i] = now.Add(t.Every)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// RunTask runs t once under its advisory lock and returns resultOK, resultSkipped (another
// replica holds the lock) or resultError.
func (h *Housekeeper) RunTask(parent context.Context, t Task) string {
	ctx, cancel := context.WithTimeout(parent, h.Timeout)
	defer cancel()
	start := h.clock.Now()
	var (
		locked bool
		n      int
	)
	err := db.WithTx(ctx, h.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, t.LockKey).Scan(&locked); err != nil || !locked {
			return err
		}
		var err error
		n, err = t.Run(ctx, tx, start)
		return err
	})
	result := resultOK
	switch {
	case err != nil:
		result = resultError
		if parent.Err() == nil { // a run cut short by shutdown is not worth an error line
			h.log.ErrorContext(ctx, "housekeeping task failed", "task", t.Name, "error", err)
		}
	case !locked:
		result = resultSkipped
		h.log.InfoContext(ctx, "housekeeping task skipped: another jobs replica holds its lock", "task", t.Name)
	default:
		h.metrics.lastSuccess.WithLabelValues(t.Name).Set(float64(h.clock.Now().Unix()))
		h.log.InfoContext(ctx, "housekeeping task done", "task", t.Name, "affected", n, "duration_ms", h.clock.Now().Sub(start).Milliseconds())
	}
	h.metrics.housekeeping.WithLabelValues(t.Name, result).Inc()
	return result
}
