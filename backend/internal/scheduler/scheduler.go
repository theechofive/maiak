// Package scheduler owns probe_locations and monitor_location_state: where and when each
// monitor is checked. The monitors module initialises and restarts schedules only through this
// service, inside its own transaction, so a monitor and its schedule commit together. The
// worker claims due jobs, completes and releases leases through it (ADR-0016).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/scheduler/schedulerdb"
)

// HomeLocationCode is the location every MVP monitor runs in (seeded by the migration).
const HomeLocationCode = "home-eu-central-1"

// MaxStartJitter spreads the first check of new and resumed monitors, so a burst of creates
// does not become a burst of checks in the same second forever after.
const MaxStartJitter = 30 * time.Second

// ErrNoLocation means the home location is missing or disabled.
var ErrNoLocation = errors.New("scheduler: home location not available")

// Service schedules monitors.
type Service struct {
	jitter func() time.Duration
}

// New returns a Service with a random start jitter in [0, MaxStartJitter).
func New() *Service {
	return &Service{jitter: func() time.Duration {
		return rand.N(MaxStartJitter) //nolint:gosec // load spreading, not security
	}}
}

// HomeLocation returns the id of the home location.
func (s *Service) HomeLocation(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	id, err := schedulerdb.New(tx).GetActiveLocationIDByCode(ctx, HomeLocationCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNoLocation
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("scheduler: home location: %w", err)
	}
	return id, nil
}

// InitMonitorLocation creates the schedule of a new (monitor, location): status unknown, first
// run after a random jitter. tx must be the tenant transaction that created the monitor.
func (s *Service) InitMonitorLocation(ctx context.Context, tx pgx.Tx, tenantID, monitorID, locationID uuid.UUID) error {
	err := schedulerdb.New(tx).InsertLocationState(ctx, schedulerdb.InsertLocationStateParams{
		TenantID: tenantID, MonitorID: monitorID, LocationID: locationID, JitterSeconds: s.jitter().Seconds(),
	})
	if err != nil {
		return fmt.Errorf("scheduler: init monitor location: %w", err)
	}
	return nil
}

// Restart resets every location of a resumed monitor: counters to zero, status unknown, any
// lease dropped and the next run after a random jitter.
func (s *Service) Restart(ctx context.Context, tx pgx.Tx, tenantID, monitorID uuid.UUID) error {
	_, err := schedulerdb.New(tx).RestartMonitorLocations(ctx, schedulerdb.RestartMonitorLocationsParams{
		TenantID: tenantID, MonitorID: monitorID, JitterSeconds: s.jitter().Seconds(),
	})
	if err != nil {
		return fmt.Errorf("scheduler: restart monitor: %w", err)
	}
	return nil
}

// LocationState is the check state of one (monitor, location).
type LocationState struct {
	LocationID          uuid.UUID
	Status              string
	ConsecutiveFailures int
	LastCheckedAt       *time.Time
}

// LocationStates returns the state of every location of a monitor. The incidents service reads
// them in the result transaction to apply the quorum rule.
func (s *Service) LocationStates(ctx context.Context, tx pgx.Tx, tenantID, monitorID uuid.UUID) ([]LocationState, error) {
	rows, err := schedulerdb.New(tx).ListLocationStates(ctx, schedulerdb.ListLocationStatesParams{TenantID: tenantID, MonitorID: monitorID})
	if err != nil {
		return nil, fmt.Errorf("scheduler: location states: %w", err)
	}
	out := make([]LocationState, len(rows))
	for i, r := range rows {
		out[i] = LocationState{LocationID: r.LocationID, Status: r.Status, ConsecutiveFailures: int(r.ConsecutiveFailures), LastCheckedAt: r.LastCheckedAt}
	}
	return out, nil
}

// SaveLocationState stores the state computed by the incident state machine.
func (s *Service) SaveLocationState(ctx context.Context, tx pgx.Tx, tenantID, monitorID uuid.UUID, st LocationState) error {
	err := schedulerdb.New(tx).UpdateLocationState(ctx, schedulerdb.UpdateLocationStateParams{
		TenantID: tenantID, MonitorID: monitorID, LocationID: st.LocationID,
		Status: st.Status, ConsecutiveFailures: int32(st.ConsecutiveFailures), LastCheckedAt: st.LastCheckedAt, //nolint:gosec // small counter
	})
	if err != nil {
		return fmt.Errorf("scheduler: save location state: %w", err)
	}
	return nil
}

// Lease identifies a claimed (monitor, location); Token fences off stale writers.
type Lease struct {
	TenantID   uuid.UUID
	MonitorID  uuid.UUID
	LocationID uuid.UUID
	Token      uuid.UUID
	Until      time.Time
}

// Job is a claimed check with what is needed to run it.
type Job struct {
	Lease
	URL              string
	ExpectedStatus   string
	FailureThreshold int
	Interval         time.Duration
	Timeout          time.Duration
	// NextRunAt is when the job was due; now minus NextRunAt is the scheduler lag.
	NextRunAt time.Time
}

// Claim leases up to n due jobs across all tenants through claim_due_monitors (a SECURITY
// DEFINER function, so it runs outside a tenant transaction).
func (s *Service) Claim(ctx context.Context, pool *db.Pool, n int) ([]Job, error) {
	var jobs []Job
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, monitor_id, location_id, lease_token, lease_until, url, expected_status,
			failure_threshold, interval_seconds, timeout_seconds, next_run_at FROM claim_due_monitors($1)`, n)
		if err != nil {
			return err
		}
		jobs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) {
			var j Job
			var interval, timeout int32
			err := r.Scan(&j.TenantID, &j.MonitorID, &j.LocationID, &j.Token, &j.Until, &j.URL, &j.ExpectedStatus,
				&j.FailureThreshold, &interval, &timeout, &j.NextRunAt)
			j.Interval, j.Timeout = time.Duration(interval)*time.Second, time.Duration(timeout)*time.Second
			return j, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("scheduler: claim: %w", err)
	}
	return jobs, nil
}

// Complete checks that l is still held (token matches and not expired), schedules the next run
// at nextRunAt and releases the lease. It returns false for a stale lease; the caller must then
// discard the result. tx must be a tenant transaction of l.TenantID.
func (s *Service) Complete(ctx context.Context, tx pgx.Tx, l Lease, nextRunAt time.Time) (bool, error) {
	n, err := schedulerdb.New(tx).CompleteLease(ctx, schedulerdb.CompleteLeaseParams{
		TenantID: l.TenantID, MonitorID: l.MonitorID, LocationID: l.LocationID, LeaseToken: &l.Token, NextRunAt: nextRunAt,
	})
	if err != nil {
		return false, fmt.Errorf("scheduler: complete lease: %w", err)
	}
	return n == 1, nil
}

// Release gives a claimed job back without checking it; it is due again after delay (or at its
// next_run_at if that is later). tx must be a tenant transaction of l.TenantID.
func (s *Service) Release(ctx context.Context, tx pgx.Tx, l Lease, delay time.Duration) error {
	if _, err := schedulerdb.New(tx).ReleaseLease(ctx, schedulerdb.ReleaseLeaseParams{
		TenantID: l.TenantID, MonitorID: l.MonitorID, LocationID: l.LocationID, LeaseToken: &l.Token,
		DelaySeconds: delay.Seconds(),
	}); err != nil {
		return fmt.Errorf("scheduler: release lease: %w", err)
	}
	return nil
}
