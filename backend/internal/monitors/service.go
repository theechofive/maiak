// Package monitors owns monitors, monitor_locations and monitor_state: creating, editing,
// pausing, resuming and (soft) deleting HTTPS monitors under the tenant limit. Schedules
// (monitor_location_state) belong to internal/scheduler and are written through its service in
// the same transaction (ADR-0016 lifecycle).
package monitors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/audit"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors/monitorsdb"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/tenants"
)

// Statuses of a monitor.
const (
	StatusUnknown = "unknown"
	StatusUp      = "up"
	StatusDown    = "down"
)

// Audit actions written by this module.
const (
	ActionCreated = "monitor.created"
	ActionUpdated = "monitor.updated"
	ActionDeleted = "monitor.deleted"
	ActionPaused  = "monitor.paused"
	ActionResumed = "monitor.resumed"
)

const typeHTTP = "http"

// ErrNotFound means no live monitor with that id exists in the tenant.
var ErrNotFound = errors.New("monitors: not found")

// LimitError means the tenant already has its maximum number of monitors.
type LimitError struct{ Limit int }

func (e *LimitError) Error() string {
	return fmt.Sprintf("monitors: limit of %d monitors reached", e.Limit)
}

// Actor is who performs a change; it scopes every operation to one tenant.
type Actor struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	IP       netip.Addr
}

// Monitor is a monitor with its aggregated state.
type Monitor struct {
	ID                uuid.UUID
	Name              string
	URL               string
	ExpectedStatus    string
	IntervalSeconds   int
	TimeoutSeconds    int
	FailureThreshold  int
	NotifySlack       bool
	Paused            bool
	Status            string
	StatusSince       time.Time
	LastErrorKind     *string
	LastCheckedAt     *time.Time
	TLSExpiresAt      *time.Time
	CurrentIncidentID *uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Summary counts monitors by display state; a paused monitor counts only as paused.
type Summary struct {
	Up, Down, Paused, Unknown int
}

// CreateInput is a new monitor. Nil fields take their defaults.
type CreateInput struct {
	Name             string
	URL              string
	IntervalSeconds  *int
	TimeoutSeconds   *int
	ExpectedStatus   *string
	FailureThreshold *int
	NotifySlack      *bool
}

// UpdateInput changes the non-nil fields.
type UpdateInput struct {
	Name             *string
	URL              *string
	IntervalSeconds  *int
	TimeoutSeconds   *int
	ExpectedStatus   *string
	FailureThreshold *int
	NotifySlack      *bool
}

// httpConfig is monitors.config for type http.
type httpConfig struct {
	URL            string `json:"url"`
	ExpectedStatus string `json:"expectedStatus"`
}

// Service implements the monitor lifecycle.
type Service struct {
	pool      *db.Pool
	sched     *scheduler.Service
	incidents *incidents.Service
}

// NewService returns a Service.
func NewService(pool *db.Pool, sched *scheduler.Service, inc *incidents.Service) *Service {
	return &Service{pool: pool, sched: sched, incidents: inc}
}

// Create validates in, enforces the tenant's monitor limit and creates the monitor with its
// location, schedule and state rows in one transaction.
func (s *Service) Create(ctx context.Context, a Actor, in CreateInput) (Monitor, error) {
	var verr validation.Error
	name, msg := ValidateName(in.Name)
	if msg != "" {
		verr.Add("name", msg)
	}
	target, msg := ValidateURL(in.URL)
	if msg != "" {
		verr.Add("url", msg)
	}
	checkFixed(&verr, "intervalSeconds", in.IntervalSeconds, IntervalSeconds)
	checkFixed(&verr, "timeoutSeconds", in.TimeoutSeconds, TimeoutSeconds)
	expected := orDefault(in.ExpectedStatus, DefaultExpectedStatus)
	if msg := ValidateExpectedStatus(expected); msg != "" {
		verr.Add("expectedStatus", msg)
	}
	threshold := orDefault(in.FailureThreshold, DefaultFailureThreshold)
	checkThreshold(&verr, threshold)
	if err := verr.Err(); err != nil {
		return Monitor{}, err
	}
	config, err := json.Marshal(httpConfig{URL: target, ExpectedStatus: expected})
	if err != nil {
		return Monitor{}, fmt.Errorf("monitors: config: %w", err)
	}

	id := uuidv7.New()
	var m Monitor
	err = db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		// The tenant row lock serialises concurrent creates, so the count below stays true
		// until this transaction commits.
		limits, err := tenants.LockLimits(ctx, tx, a.TenantID)
		if err != nil {
			return err
		}
		q := monitorsdb.New(tx)
		n, err := q.CountLiveMonitors(ctx, a.TenantID)
		if err != nil {
			return fmt.Errorf("count monitors: %w", err)
		}
		if n >= int64(limits.MaxMonitors) {
			return &LimitError{Limit: limits.MaxMonitors}
		}
		location, err := s.sched.HomeLocation(ctx, tx)
		if err != nil {
			return err
		}
		if err := q.InsertMonitor(ctx, monitorsdb.InsertMonitorParams{
			ID: id, TenantID: a.TenantID, Name: name, Type: typeHTTP, Config: config,
			IntervalSeconds: IntervalSeconds, TimeoutSeconds: TimeoutSeconds,
			FailureThreshold: int32(threshold), NotifySlack: orDefault(in.NotifySlack, true), //nolint:gosec // 1..3
		}); err != nil {
			return fmt.Errorf("insert monitor: %w", err)
		}
		if err := q.InsertMonitorLocation(ctx, monitorsdb.InsertMonitorLocationParams{TenantID: a.TenantID, MonitorID: id, LocationID: location}); err != nil {
			return fmt.Errorf("insert monitor location: %w", err)
		}
		if err := s.sched.InitMonitorLocation(ctx, tx, a.TenantID, id, location); err != nil {
			return err
		}
		if err := q.InsertMonitorState(ctx, monitorsdb.InsertMonitorStateParams{TenantID: a.TenantID, MonitorID: id}); err != nil {
			return fmt.Errorf("insert monitor state: %w", err)
		}
		if err := s.audit(ctx, tx, a, ActionCreated, id, nil); err != nil {
			return err
		}
		m, err = get(ctx, q, a.TenantID, id)
		return err
	})
	if err != nil {
		return Monitor{}, wrap("create", err)
	}
	return m, nil
}

// List returns the live monitors of the tenant, oldest first, and the summary counts.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]Monitor, Summary, error) {
	var out []Monitor
	var sum Summary
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := monitorsdb.New(tx).ListMonitors(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("list monitors: %w", err)
		}
		out = make([]Monitor, 0, len(rows))
		for _, r := range rows {
			m, err := fromRow(monitorsdb.GetMonitorRow(r))
			if err != nil {
				return err
			}
			switch {
			case m.Paused:
				sum.Paused++
			case m.Status == StatusUp:
				sum.Up++
			case m.Status == StatusDown:
				sum.Down++
			default:
				sum.Unknown++
			}
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		return nil, Summary{}, wrap("list", err)
	}
	return out, sum, nil
}

// Get returns one live monitor.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Monitor, error) {
	var m Monitor
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		m, err = get(ctx, monitorsdb.New(tx), tenantID, id)
		return err
	})
	if err != nil {
		return Monitor{}, wrap("get", err)
	}
	return m, nil
}

// Update changes the given fields. Interval and timeout are fixed in the MVP: they may be sent
// only with their current value.
func (s *Service) Update(ctx context.Context, a Actor, id uuid.UUID, in UpdateInput) (Monitor, error) {
	var m Monitor
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		q := monitorsdb.New(tx)
		cur, err := q.LockMonitor(ctx, monitorsdb.LockMonitorParams{TenantID: a.TenantID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock monitor: %w", err)
		}
		var cfg httpConfig
		if err := json.Unmarshal(cur.Config, &cfg); err != nil {
			return fmt.Errorf("monitor config: %w", err)
		}
		name, threshold, notify := cur.Name, int(cur.FailureThreshold), cur.NotifySlack

		var verr validation.Error
		var changed []string
		if in.Name != nil {
			v, msg := ValidateName(*in.Name)
			if msg != "" {
				verr.Add("name", msg)
			}
			changed, name = appendIf(changed, "name", v != name), v
		}
		if in.URL != nil {
			v, msg := ValidateURL(*in.URL)
			if msg != "" {
				verr.Add("url", msg)
			}
			changed, cfg.URL = appendIf(changed, "url", v != cfg.URL), v
		}
		checkFixed(&verr, "intervalSeconds", in.IntervalSeconds, int(cur.IntervalSeconds))
		checkFixed(&verr, "timeoutSeconds", in.TimeoutSeconds, int(cur.TimeoutSeconds))
		if in.ExpectedStatus != nil {
			if msg := ValidateExpectedStatus(*in.ExpectedStatus); msg != "" {
				verr.Add("expectedStatus", msg)
			}
			changed, cfg.ExpectedStatus = appendIf(changed, "expectedStatus", *in.ExpectedStatus != cfg.ExpectedStatus), *in.ExpectedStatus
		}
		if in.FailureThreshold != nil {
			checkThreshold(&verr, *in.FailureThreshold)
			changed, threshold = appendIf(changed, "failureThreshold", *in.FailureThreshold != threshold), *in.FailureThreshold
		}
		if in.NotifySlack != nil {
			changed, notify = appendIf(changed, "notifySlack", *in.NotifySlack != notify), *in.NotifySlack
		}
		if err := verr.Err(); err != nil {
			return err
		}
		if slices.Contains(changed, "url") {
			// A new URL is a new target: its history says nothing about it. Restart like resume
			// and end an open incident without a notification (ADR-0018).
			if err := s.restart(ctx, tx, q, a.TenantID, id, true); err != nil {
				return err
			}
			if err := s.incidents.ResolveOpen(ctx, tx, a.TenantID, id, incidents.ReasonURLChanged); err != nil {
				return err
			}
		}
		if len(changed) > 0 {
			config, err := json.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("monitor config: %w", err)
			}
			if err := q.UpdateMonitor(ctx, monitorsdb.UpdateMonitorParams{
				TenantID: a.TenantID, ID: id, Name: name, Config: config,
				FailureThreshold: int32(threshold), NotifySlack: notify, //nolint:gosec // validated 1..3
			}); err != nil {
				return fmt.Errorf("update monitor: %w", err)
			}
			// Only field names: the URL may carry tokens in its query string.
			if err := s.audit(ctx, tx, a, ActionUpdated, id, map[string]any{"fields": changed}); err != nil {
				return err
			}
		}
		m, err = get(ctx, q, a.TenantID, id)
		return err
	})
	if err != nil {
		return Monitor{}, wrap("update", err)
	}
	return m, nil
}

// Delete soft-deletes a monitor. It stops being scheduled (the claim skips deleted monitors)
// and no longer counts toward the limit; its history stays until retention.
func (s *Service) Delete(ctx context.Context, a Actor, id uuid.UUID) error {
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		q := monitorsdb.New(tx)
		if _, err := q.LockMonitor(ctx, monitorsdb.LockMonitorParams{TenantID: a.TenantID, ID: id}); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("lock monitor: %w", err)
		}
		if err := s.incidents.ResolveOpen(ctx, tx, a.TenantID, id, incidents.ReasonDeleted); err != nil {
			return err
		}
		if err := q.SoftDeleteMonitor(ctx, monitorsdb.SoftDeleteMonitorParams{TenantID: a.TenantID, ID: id}); err != nil {
			return fmt.Errorf("delete monitor: %w", err)
		}
		return s.audit(ctx, tx, a, ActionDeleted, id, nil)
	})
	return wrap("delete", err)
}

// Pause stops checking a monitor: an open incident is resolved without a notification, a check
// in flight is discarded and the status becomes unknown. Pausing a paused monitor changes nothing.
func (s *Service) Pause(ctx context.Context, a Actor, id uuid.UUID) (Monitor, error) {
	return s.setPaused(ctx, a, id, true)
}

// Resume starts checking a paused monitor again: counters reset, status unknown until the first
// check, which runs after a random jitter. Resuming a running monitor changes nothing.
func (s *Service) Resume(ctx context.Context, a Actor, id uuid.UUID) (Monitor, error) {
	return s.setPaused(ctx, a, id, false)
}

func (s *Service) setPaused(ctx context.Context, a Actor, id uuid.UUID, paused bool) (Monitor, error) {
	var m Monitor
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		q := monitorsdb.New(tx)
		cur, err := q.LockMonitor(ctx, monitorsdb.LockMonitorParams{TenantID: a.TenantID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock monitor: %w", err)
		}
		if cur.Paused != paused {
			if err := q.SetMonitorPaused(ctx, monitorsdb.SetMonitorPausedParams{TenantID: a.TenantID, ID: id, Paused: paused}); err != nil {
				return fmt.Errorf("set paused: %w", err)
			}
			// Both directions restart the runtime state. On pause this drops the lease, which
			// fences off a check still in flight, and the status becomes unknown (a paused monitor
			// reports unknown); on resume the first check comes after a random jitter (ADR-0040).
			if err := s.restart(ctx, tx, q, a.TenantID, id, false); err != nil {
				return err
			}
			action := ActionResumed
			if paused {
				action = ActionPaused
				if err := s.incidents.ResolveOpen(ctx, tx, a.TenantID, id, incidents.ReasonPaused); err != nil {
					return err
				}
			}
			if err := s.audit(ctx, tx, a, action, id, nil); err != nil {
				return err
			}
		}
		m, err = get(ctx, q, a.TenantID, id)
		return err
	})
	if err != nil {
		return Monitor{}, wrap("set paused", err)
	}
	return m, nil
}

// restart resets the schedule (through the scheduler) and the aggregated state of a monitor;
// clearTLS also forgets the certificate seen so far (URL change).
func (s *Service) restart(ctx context.Context, tx pgx.Tx, q *monitorsdb.Queries, tenantID, id uuid.UUID, clearTLS bool) error {
	if err := s.sched.Restart(ctx, tx, id); err != nil {
		return err
	}
	if err := q.ResetMonitorState(ctx, monitorsdb.ResetMonitorStateParams{TenantID: tenantID, MonitorID: id, ClearTls: clearTLS}); err != nil {
		return fmt.Errorf("reset monitor state: %w", err)
	}
	return nil
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, a Actor, action string, id uuid.UUID, meta map[string]any) error {
	actor := a.UserID
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: a.TenantID, ActorUserID: &actor, Action: action,
		EntityType: "monitor", EntityID: id.String(), Metadata: meta, IP: a.IP,
	})
}

func get(ctx context.Context, q *monitorsdb.Queries, tenantID, id uuid.UUID) (Monitor, error) {
	row, err := q.GetMonitor(ctx, monitorsdb.GetMonitorParams{TenantID: tenantID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Monitor{}, ErrNotFound
	}
	if err != nil {
		return Monitor{}, fmt.Errorf("get monitor: %w", err)
	}
	return fromRow(row)
}

func fromRow(r monitorsdb.GetMonitorRow) (Monitor, error) {
	var cfg httpConfig
	if err := json.Unmarshal(r.Config, &cfg); err != nil {
		return Monitor{}, fmt.Errorf("monitor %s config: %w", r.ID, err)
	}
	return Monitor{
		ID: r.ID, Name: r.Name, URL: cfg.URL, ExpectedStatus: cfg.ExpectedStatus,
		IntervalSeconds: int(r.IntervalSeconds), TimeoutSeconds: int(r.TimeoutSeconds),
		FailureThreshold: int(r.FailureThreshold), NotifySlack: r.NotifySlack, Paused: r.Paused,
		Status: r.Status, StatusSince: r.StatusChangedAt, LastErrorKind: r.LastErrorKind,
		LastCheckedAt: r.LastCheckedAt, TLSExpiresAt: r.TlsExpiresAt, CurrentIncidentID: r.CurrentIncidentID,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}, nil
}

// wrap adds context to unexpected errors and leaves domain errors recognisable.
func wrap(op string, err error) error {
	var verr *validation.Error
	var lerr *LimitError
	if err == nil || errors.Is(err, ErrNotFound) || errors.As(err, &verr) || errors.As(err, &lerr) {
		return err
	}
	return fmt.Errorf("monitors: %s: %w", op, err)
}

func checkFixed(verr *validation.Error, field string, got *int, want int) {
	if got != nil && *got != want {
		verr.Add(field, fmt.Sprintf("only %d is supported", want))
	}
}

func checkThreshold(verr *validation.Error, n int) {
	if n < 1 || n > 3 {
		verr.Add("failureThreshold", "must be between 1 and 3")
	}
}

func appendIf(list []string, item string, cond bool) []string {
	if cond {
		return append(list, item)
	}
	return list
}

func orDefault[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
