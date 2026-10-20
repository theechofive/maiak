// Package scheduler owns probe_locations and monitor_location_state: where and when each
// monitor is checked. The monitors module initialises and restarts schedules only through this
// service, inside its own transaction, so a monitor and its schedule commit together. The
// worker's claim loop (B-12) will live here too.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
