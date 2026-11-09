// Package monitors owns monitors, monitor_locations, monitor_state and heartbeat_tokens:
// creating, editing, pausing, resuming and (soft) deleting HTTPS and heartbeat monitors under the
// tenant's plan quotas, heartbeat pings and their evaluation (ADR-0094). Schedules
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
	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors/monitorsdb"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
	"github.com/theechofive/maiak/backend/internal/projects"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/statuspages"
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
	// ActionTokenRegenerated: a heartbeat got a new URL (the token is never recorded).
	ActionTokenRegenerated = "monitor.heartbeat_token_regenerated"
)

// Monitor types (monitors.type).
const (
	TypeHTTP      = "http"
	TypeHeartbeat = "heartbeat"
)

var (
	// ErrNotFound means no live monitor with that id exists in the tenant.
	ErrNotFound = errors.New("monitors: not found")
	// ErrNotHeartbeat means the operation exists only for heartbeat monitors.
	ErrNotHeartbeat = errors.New("monitors: not a heartbeat monitor")
)

// Actor is who performs a change; it scopes every operation to one tenant, and Access to the
// projects the user may change monitors in (ADR-0102, T-02).
type Actor struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	IP       netip.Addr
	Access   authz.Actor
}

// checkAccess refuses a change of a monitor in project: ErrNotFound when the actor does not see
// the project (so ids of other projects reveal nothing), authz.ErrProjectForbidden when its role
// there is below member.
func (a Actor) checkAccess(project uuid.UUID) error {
	if !a.Access.Sees(project) {
		return ErrNotFound
	}
	if !a.Access.CanEditIn(project) {
		return authz.ErrProjectForbidden
	}
	return nil
}

// Monitor is a monitor with its aggregated state.
type Monitor struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	Type      string
	Name      string
	// URL and ExpectedStatus are empty for a heartbeat.
	URL            string
	ExpectedStatus string
	// IntervalSeconds is the check interval, or a heartbeat's expected ping interval.
	IntervalSeconds   int
	TimeoutSeconds    int
	GraceSeconds      int // heartbeats only
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
	// HeartbeatToken is the secret of a new heartbeat's URL, set only in the result of Create. It
	// is never stored (only its hash is) and cannot be read again.
	HeartbeatToken string
}

// CreateInput is a new monitor. Nil fields take their defaults. Type is TypeHTTP when empty;
// URL and ExpectedStatus belong to http monitors, ExpectedIntervalSeconds and GraceSeconds to
// heartbeats. A heartbeat ignores IntervalSeconds, TimeoutSeconds and FailureThreshold (its
// failure threshold is always 1).
type CreateInput struct {
	Type                    string
	Name                    string
	URL                     *string
	IntervalSeconds         *int
	TimeoutSeconds          *int
	ExpectedStatus          *string
	FailureThreshold        *int
	NotifySlack             *bool
	ExpectedIntervalSeconds *int
	GraceSeconds            *int
	// ProjectID is the monitor's project; nil means the Default project.
	ProjectID *uuid.UUID
	// ChannelIDs selects notification channels of the project or organization-wide ones (none:
	// the active channels of the project, ADR-0100, T-02).
	ChannelIDs *[]uuid.UUID
}

// UpdateInput changes the non-nil fields (the same per-type rules as CreateInput).
type UpdateInput struct {
	Name                    *string
	URL                     *string
	IntervalSeconds         *int
	TimeoutSeconds          *int
	ExpectedStatus          *string
	FailureThreshold        *int
	NotifySlack             *bool
	ExpectedIntervalSeconds *int
	GraceSeconds            *int
	// ProjectID moves the monitor; selected channels of the old project are dropped.
	ProjectID *uuid.UUID
	// ChannelIDs replaces the channel selection.
	ChannelIDs *[]uuid.UUID
}

// httpConfig is monitors.config for type http.
type httpConfig struct {
	URL            string `json:"url"`
	ExpectedStatus string `json:"expectedStatus"`
}

// heartbeatConfig is monitors.config for type heartbeat.
type heartbeatConfig struct {
	GraceSeconds int `json:"graceSeconds"`
}

const (
	msgHTTPOnly      = "is only for http monitors"
	msgHeartbeatOnly = "is only for heartbeat monitors"
)

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

// Create validates in, enforces the tenant's quota for the type (a *plans.LimitError when it is
// used up) and creates the monitor with its location, schedule and state rows (and, for a
// heartbeat, its token) in one transaction.
func (s *Service) Create(ctx context.Context, a Actor, in CreateInput) (Monitor, error) {
	var verr validation.Error
	name, msg := ValidateName(in.Name)
	if msg != "" {
		verr.Add("name", msg)
	}
	var row monitorsdb.InsertMonitorParams
	var config any
	resource := plans.ResourceMonitor
	switch in.Type {
	case "", TypeHTTP:
		row.Type = TypeHTTP
		var target string
		if in.URL == nil {
			verr.Add("url", "is required")
		} else if target, msg = ValidateURL(*in.URL); msg != "" {
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
		rejectSet(&verr, msgHeartbeatOnly, field{"expectedIntervalSeconds", in.ExpectedIntervalSeconds != nil}, field{"graceSeconds", in.GraceSeconds != nil})
		config = httpConfig{URL: target, ExpectedStatus: expected}
		row.IntervalSeconds, row.FailureThreshold = IntervalSeconds, int32(threshold) //nolint:gosec // validated 1..3
	case TypeHeartbeat:
		row.Type, resource = TypeHeartbeat, plans.ResourceHeartbeat
		interval := orDefault(in.ExpectedIntervalSeconds, DefaultExpectedIntervalSeconds)
		if msg := ValidateExpectedInterval(interval); msg != "" {
			verr.Add("expectedIntervalSeconds", msg)
		}
		grace := orDefault(in.GraceSeconds, DefaultGraceSeconds)
		if msg := ValidateGrace(grace); msg != "" {
			verr.Add("graceSeconds", msg)
		}
		// intervalSeconds and timeoutSeconds are ignored like failureThreshold: the contract's
		// defaults fill them into every create request.
		rejectSet(&verr, msgHTTPOnly, field{"url", in.URL != nil}, field{"expectedStatus", in.ExpectedStatus != nil})
		config = heartbeatConfig{GraceSeconds: grace}
		row.IntervalSeconds, row.FailureThreshold = int32(interval), 1 //nolint:gosec // validated 60..86400
	default:
		verr.Add("type", `must be "http" or "heartbeat"`)
	}
	if err := verr.Err(); err != nil {
		return Monitor{}, err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return Monitor{}, fmt.Errorf("monitors: config: %w", err)
	}
	var token string
	var hash []byte
	if row.Type == TypeHeartbeat {
		if token, hash, err = NewHeartbeatToken(); err != nil {
			return Monitor{}, err
		}
	}

	id := uuidv7.New()
	row.ID, row.TenantID, row.Name, row.Config = id, a.TenantID, name, raw
	row.TimeoutSeconds, row.NotifySlack = TimeoutSeconds, orDefault(in.NotifySlack, true)
	var m Monitor
	err = db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		// The tenant row lock taken here serialises concurrent creates, so the count stays true
		// until this transaction commits.
		if err := plans.CheckCreate(ctx, tx, a.TenantID, resource); err != nil {
			return err
		}
		q := monitorsdb.New(tx)
		project, err := projects.ForUse(ctx, tx, a.TenantID, in.ProjectID)
		if errors.Is(err, projects.ErrNotFound) || (err == nil && !a.Access.Sees(project)) {
			return projectFieldError()
		}
		if err != nil {
			return err
		}
		if !a.Access.CanEditIn(project) {
			return authz.ErrProjectForbidden
		}
		row.ProjectID = project
		location, err := s.sched.HomeLocation(ctx, tx)
		if err != nil {
			return err
		}
		if err := q.InsertMonitor(ctx, row); err != nil {
			return fmt.Errorf("insert monitor: %w", err)
		}
		if err := q.InsertMonitorLocation(ctx, monitorsdb.InsertMonitorLocationParams{TenantID: a.TenantID, MonitorID: id, LocationID: location}); err != nil {
			return fmt.Errorf("insert monitor location: %w", err)
		}
		if row.Type == TypeHeartbeat {
			// The location state only carries the incident machine's state; nothing claims it.
			if err := s.sched.InitParkedMonitorLocation(ctx, tx, a.TenantID, id, location); err != nil {
				return err
			}
			if err := q.InsertHeartbeatToken(ctx, monitorsdb.InsertHeartbeatTokenParams{TenantID: a.TenantID, MonitorID: id, TokenHash: hash}); err != nil {
				return fmt.Errorf("insert heartbeat token: %w", err)
			}
		} else if err := s.sched.InitMonitorLocation(ctx, tx, a.TenantID, id, location); err != nil {
			return err
		}
		if err := q.InsertMonitorState(ctx, monitorsdb.InsertMonitorStateParams{TenantID: a.TenantID, MonitorID: id}); err != nil {
			return fmt.Errorf("insert monitor state: %w", err)
		}
		if in.ChannelIDs != nil {
			if _, err := notify.SetMonitorChannels(ctx, tx, a.TenantID, id, project, *in.ChannelIDs); err != nil {
				return err
			}
		}
		if err := s.audit(ctx, tx, a, ActionCreated, id, map[string]any{"type": row.Type, "projectId": project.String()}); err != nil {
			return err
		}
		m, err = get(ctx, q, a.TenantID, id)
		return err
	})
	if err != nil {
		return Monitor{}, wrap("create", err)
	}
	m.HeartbeatToken = token
	return m, nil
}

// Update changes the given fields. The interval and timeout of an http monitor are fixed in the
// MVP: they may be sent only with their current value. A heartbeat's interval and grace may
// change; its deadline then follows the new values, and FailureThreshold is ignored.
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
		if err := a.checkAccess(cur.ProjectID); err != nil {
			return err
		}
		name, interval, threshold, notifySlack := cur.Name, int(cur.IntervalSeconds), int(cur.FailureThreshold), cur.NotifySlack

		var verr validation.Error
		var changed []string
		if in.Name != nil {
			v, msg := ValidateName(*in.Name)
			if msg != "" {
				verr.Add("name", msg)
			}
			changed, name = appendIf(changed, "name", v != name), v
		}
		if in.NotifySlack != nil {
			changed, notifySlack = appendIf(changed, "notifySlack", *in.NotifySlack != notifySlack), *in.NotifySlack
		}
		checkFixed(&verr, "intervalSeconds", in.IntervalSeconds, int(cur.IntervalSeconds))
		checkFixed(&verr, "timeoutSeconds", in.TimeoutSeconds, int(cur.TimeoutSeconds))

		var config any
		if cur.Type == TypeHeartbeat {
			var cfg heartbeatConfig
			if err := json.Unmarshal(cur.Config, &cfg); err != nil {
				return fmt.Errorf("monitor config: %w", err)
			}
			rejectSet(&verr, msgHTTPOnly, field{"url", in.URL != nil}, field{"expectedStatus", in.ExpectedStatus != nil})
			if in.ExpectedIntervalSeconds != nil {
				if msg := ValidateExpectedInterval(*in.ExpectedIntervalSeconds); msg != "" {
					verr.Add("expectedIntervalSeconds", msg)
				}
				changed, interval = appendIf(changed, "expectedIntervalSeconds", *in.ExpectedIntervalSeconds != interval), *in.ExpectedIntervalSeconds
			}
			if in.GraceSeconds != nil {
				if msg := ValidateGrace(*in.GraceSeconds); msg != "" {
					verr.Add("graceSeconds", msg)
				}
				changed, cfg.GraceSeconds = appendIf(changed, "graceSeconds", *in.GraceSeconds != cfg.GraceSeconds), *in.GraceSeconds
			}
			config = cfg
		} else {
			var cfg httpConfig
			if err := json.Unmarshal(cur.Config, &cfg); err != nil {
				return fmt.Errorf("monitor config: %w", err)
			}
			rejectSet(&verr, msgHeartbeatOnly, field{"expectedIntervalSeconds", in.ExpectedIntervalSeconds != nil}, field{"graceSeconds", in.GraceSeconds != nil})
			if in.URL != nil {
				v, msg := ValidateURL(*in.URL)
				if msg != "" {
					verr.Add("url", msg)
				}
				changed, cfg.URL = appendIf(changed, "url", v != cfg.URL), v
			}
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
			config = cfg
		}
		if err := verr.Err(); err != nil {
			return err
		}
		project := cur.ProjectID
		var moveMeta map[string]any
		if in.ProjectID != nil && *in.ProjectID != project {
			target, err := projects.ForUse(ctx, tx, a.TenantID, in.ProjectID)
			if errors.Is(err, projects.ErrNotFound) || (err == nil && !a.Access.Sees(target)) {
				return projectFieldError()
			}
			if err != nil {
				return err
			}
			if !a.Access.CanEditIn(target) {
				return authz.ErrProjectForbidden
			}
			if err := q.SetMonitorProject(ctx, monitorsdb.SetMonitorProjectParams{TenantID: a.TenantID, ID: id, ProjectID: target}); err != nil {
				return fmt.Errorf("move monitor: %w", err)
			}
			// Channels of the old project are not allowed in the new one (ADR-0102, T-02).
			detached, err := notify.DetachForeignChannels(ctx, tx, a.TenantID, id, target)
			if err != nil {
				return err
			}
			moveMeta = map[string]any{"from": project.String(), "to": target.String(), "channelsDetached": detached}
			changed, project = append(changed, "projectId"), target
		}
		if in.ChannelIDs != nil {
			selected, err := notify.SetMonitorChannels(ctx, tx, a.TenantID, id, project, *in.ChannelIDs)
			if err != nil {
				return err
			}
			changed = appendIf(changed, "channelIds", selected)
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
			raw, err := json.Marshal(config)
			if err != nil {
				return fmt.Errorf("monitor config: %w", err)
			}
			if err := q.UpdateMonitor(ctx, monitorsdb.UpdateMonitorParams{
				TenantID: a.TenantID, ID: id, Name: name, Config: raw, IntervalSeconds: int32(interval), //nolint:gosec // validated
				FailureThreshold: int32(threshold), NotifySlack: notifySlack, //nolint:gosec // validated 1..3
			}); err != nil {
				return fmt.Errorf("update monitor: %w", err)
			}
			// Only field names: the URL may carry tokens in its query string.
			meta := map[string]any{"fields": changed}
			if moveMeta != nil {
				meta["move"] = moveMeta
			}
			if err := s.audit(ctx, tx, a, ActionUpdated, id, meta); err != nil {
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
// and no longer counts toward the quota; its history stays until retention.
func (s *Service) Delete(ctx context.Context, a Actor, id uuid.UUID) error {
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		q := monitorsdb.New(tx)
		cur, err := q.LockMonitor(ctx, monitorsdb.LockMonitorParams{TenantID: a.TenantID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("lock monitor: %w", err)
		}
		if err := a.checkAccess(cur.ProjectID); err != nil {
			return err
		}
		if cur.Type == TypeHeartbeat {
			if err := lockHeartbeat(ctx, q, a.TenantID, id); err != nil {
				return err
			}
		}
		if err := s.incidents.ResolveOpen(ctx, tx, a.TenantID, id, incidents.ReasonDeleted); err != nil {
			return err
		}
		if err := q.SoftDeleteMonitor(ctx, monitorsdb.SoftDeleteMonitorParams{TenantID: a.TenantID, ID: id}); err != nil {
			return fmt.Errorf("delete monitor: %w", err)
		}
		// A published status page drops the component at once (ADR-0103, P-06).
		if err := statuspages.RefreshForMonitor(ctx, tx, a.TenantID, id, time.Now()); err != nil {
			return err
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
		if err := a.checkAccess(cur.ProjectID); err != nil {
			return err
		}
		if cur.Paused != paused {
			if cur.Type == TypeHeartbeat {
				// Taken before the state rows, like the evaluation in jobs (ADR-0094).
				if err := lockHeartbeat(ctx, q, a.TenantID, id); err != nil {
					return err
				}
				// Resume: the deadline counts from now; pings from before the pause are ignored.
				if !paused {
					if err := q.RestartHeartbeatWait(ctx, monitorsdb.RestartHeartbeatWaitParams{TenantID: a.TenantID, MonitorID: id}); err != nil {
						return fmt.Errorf("restart heartbeat wait: %w", err)
					}
				}
			}
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
			// A published status page shows the paused component as "no data" at once (P-06).
			if err := statuspages.RefreshForMonitor(ctx, tx, a.TenantID, id, time.Now()); err != nil {
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
	var cfg struct {
		httpConfig
		heartbeatConfig
	}
	if err := json.Unmarshal(r.Config, &cfg); err != nil {
		return Monitor{}, fmt.Errorf("monitor %s config: %w", r.ID, err)
	}
	return Monitor{
		ID: r.ID, ProjectID: r.ProjectID, Type: r.Type, Name: r.Name, URL: cfg.URL, ExpectedStatus: cfg.ExpectedStatus,
		IntervalSeconds: int(r.IntervalSeconds), TimeoutSeconds: int(r.TimeoutSeconds), GraceSeconds: cfg.GraceSeconds,
		FailureThreshold: int(r.FailureThreshold), NotifySlack: r.NotifySlack, Paused: r.Paused,
		Status: r.Status, StatusSince: r.StatusChangedAt, LastErrorKind: r.LastErrorKind,
		LastCheckedAt: r.LastCheckedAt, TLSExpiresAt: r.TlsExpiresAt, CurrentIncidentID: r.CurrentIncidentID,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}, nil
}

// wrap adds context to unexpected errors and leaves domain errors recognisable.
func wrap(op string, err error) error {
	var verr *validation.Error
	var lerr *plans.LimitError
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotHeartbeat) || errors.Is(err, authz.ErrProjectForbidden) ||
		errors.As(err, &verr) || errors.As(err, &lerr) {
		return err
	}
	return fmt.Errorf("monitors: %s: %w", op, err)
}

func projectFieldError() error {
	var verr validation.Error
	verr.Add("projectId", "does not exist")
	return &verr
}

func checkFixed(verr *validation.Error, field string, got *int, want int) {
	if got != nil && *got != want {
		verr.Add(field, fmt.Sprintf("only %d is supported", want))
	}
}

// field is a request field and whether it was sent.
type field struct {
	name string
	set  bool
}

// rejectSet reports every sent field that the monitor's type does not have.
func rejectSet(verr *validation.Error, msg string, fields ...field) {
	for _, f := range fields {
		if f.set {
			verr.Add(f.name, msg)
		}
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
