// Package incidents owns incidents, incident_monitors and the notification outbox
// (notification_deliveries). It applies the incident state machine (package machine) to a check
// result inside the caller's transaction: per-location and aggregated state, opening and
// resolving incidents, and outbox rows, so a state change and its notification commit together
// (ADR-0007). It also writes the state-machine fields of monitor_state, which the monitors
// module created and resets on resume.
package incidents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/incidents/incidentsdb"
	"github.com/theechofive/maiak/backend/internal/incidents/machine"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/scheduler"
)

// Reasons an incident ends (incidents.resolve_reason).
const (
	ReasonRecovered = "recovered"
	ReasonPaused    = "paused"
	ReasonDeleted   = "deleted"
)

// ErrMonitorNotFound means the monitor or its state row does not exist in the tenant.
var ErrMonitorNotFound = errors.New("incidents: monitor not found")

// Service applies check results to incident state.
type Service struct {
	sched *scheduler.Service
}

// NewService returns a Service.
func NewService(sched *scheduler.Service) *Service {
	return &Service{sched: sched}
}

// ResultInput is one check result of a (monitor, location).
type ResultInput struct {
	TenantID   uuid.UUID
	MonitorID  uuid.UUID
	LocationID uuid.UUID
	Result     machine.Result
	// Now is the current time (TLS expiry is measured from it).
	Now time.Time
}

// ApplyResult runs the state machine for one result in tx, which must be a tenant transaction
// of in.TenantID. Results of paused or deleted monitors and results older than the location's
// last one change nothing (Output.Ignored).
func (s *Service) ApplyResult(ctx context.Context, tx pgx.Tx, in ResultInput) (machine.Output, error) {
	q := incidentsdb.New(tx)
	// Read after the caller inserted the result row: that insert's foreign key check waits for a
	// concurrent pause or delete (they lock the monitor row), so this read sees its outcome.
	mon, err := q.GetMonitorForResult(ctx, incidentsdb.GetMonitorForResultParams{TenantID: in.TenantID, ID: in.MonitorID})
	if errors.Is(err, pgx.ErrNoRows) {
		return machine.Output{}, ErrMonitorNotFound
	}
	if err != nil {
		return machine.Output{}, fmt.Errorf("incidents: monitor: %w", err)
	}
	if mon.Paused || mon.Deleted {
		return machine.Output{Ignored: true, Transition: machine.None}, nil
	}
	st, err := q.LockMonitorState(ctx, incidentsdb.LockMonitorStateParams{TenantID: in.TenantID, MonitorID: in.MonitorID})
	if errors.Is(err, pgx.ErrNoRows) {
		return machine.Output{}, ErrMonitorNotFound
	}
	if err != nil {
		return machine.Output{}, fmt.Errorf("incidents: lock monitor state: %w", err)
	}
	locs, err := s.sched.LocationStates(ctx, tx, in.TenantID, in.MonitorID)
	if err != nil {
		return machine.Output{}, err
	}

	mi := machine.Input{
		Monitor: machine.MonitorState{
			Status: machine.Status(st.Status), StatusChangedAt: st.StatusChangedAt, LastErrorKind: deref(st.LastErrorKind),
			LastCheckedAt: derefTime(st.LastCheckedAt), TLSExpiresAt: st.TlsExpiresAt, TLSWarnedNotAfter: st.TlsWarnedNotAfter,
			IncidentOpen: st.CurrentIncidentID != nil,
		},
		Result: in.Result,
		Config: machine.Config{FailureThreshold: int(mon.FailureThreshold), NotifySlack: mon.NotifySlack},
		Now:    in.Now,
	}
	found := false
	for _, l := range locs {
		ls := machine.LocationState{Status: machine.Status(l.Status), ConsecutiveFailures: l.ConsecutiveFailures, LastCheckedAt: derefTime(l.LastCheckedAt)}
		if l.LocationID == in.LocationID {
			mi.Location, found = ls, true
		} else {
			mi.Others = append(mi.Others, ls)
		}
	}
	if !found {
		return machine.Output{}, ErrMonitorNotFound
	}

	out := machine.Apply(mi)
	if out.Ignored {
		return out, nil
	}
	checked := out.Location.LastCheckedAt
	if err := s.sched.SaveLocationState(ctx, tx, in.TenantID, in.MonitorID, scheduler.LocationState{
		LocationID: in.LocationID, Status: string(out.Location.Status),
		ConsecutiveFailures: out.Location.ConsecutiveFailures, LastCheckedAt: &checked,
	}); err != nil {
		return machine.Output{}, err
	}

	incidentID := st.CurrentIncidentID
	var startedAt time.Time
	switch out.Transition {
	case machine.Opened:
		id := uuidv7.New()
		incidentID, startedAt = &id, in.Result.CheckedAt.UTC()
		if err := q.InsertIncident(ctx, incidentsdb.InsertIncidentParams{
			ID: id, TenantID: in.TenantID, StartedAt: startedAt, Reason: nonEmpty(in.Result.ErrorKind),
		}); err != nil {
			return machine.Output{}, fmt.Errorf("incidents: open: %w", err)
		}
		if err := q.InsertIncidentMonitor(ctx, incidentsdb.InsertIncidentMonitorParams{TenantID: in.TenantID, IncidentID: id, MonitorID: in.MonitorID}); err != nil {
			return machine.Output{}, fmt.Errorf("incidents: link monitor: %w", err)
		}
	case machine.Resolved:
		if incidentID != nil {
			res, err := q.ResolveIncident(ctx, incidentsdb.ResolveIncidentParams{
				TenantID: in.TenantID, ID: *incidentID, ResolvedAt: &in.Result.CheckedAt, ResolveReason: nonEmpty(ReasonRecovered),
			})
			if err != nil {
				return machine.Output{}, fmt.Errorf("incidents: resolve: %w", err)
			}
			startedAt = res.StartedAt.UTC()
		}
	}

	m := out.Monitor
	var current *uuid.UUID
	if m.IncidentOpen {
		current = incidentID
	}
	lastChecked := m.LastCheckedAt
	if err := q.UpdateMonitorState(ctx, incidentsdb.UpdateMonitorStateParams{
		TenantID: in.TenantID, MonitorID: in.MonitorID, Status: string(m.Status), StatusChangedAt: m.StatusChangedAt,
		LastErrorKind: nonEmpty(m.LastErrorKind), LastCheckedAt: &lastChecked, TlsExpiresAt: m.TLSExpiresAt,
		TlsWarnedNotAfter: m.TLSWarnedNotAfter, CurrentIncidentID: current,
	}); err != nil {
		return machine.Output{}, fmt.Errorf("incidents: update monitor state: %w", err)
	}

	if len(out.Emit) > 0 {
		p := payloadBase{MonitorID: in.MonitorID, MonitorName: mon.Name, URL: displayURL(mon.Config)}
		if err := s.enqueue(ctx, q, in, out, p, incidentID, startedAt); err != nil {
			return machine.Output{}, err
		}
	}
	return out, nil
}

// enqueue writes one outbox row per emitted kind, but only when the tenant has an active Slack
// channel (ADR-0019): without one there is nobody to deliver to.
func (s *Service) enqueue(ctx context.Context, q *incidentsdb.Queries, in ResultInput, out machine.Output, base payloadBase,
	incidentID *uuid.UUID, startedAt time.Time) error {
	channel, err := q.GetActiveSlackChannel(ctx, in.TenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("incidents: slack channel: %w", err)
	}
	for _, kind := range out.Emit {
		p := payload{payloadBase: base}
		var incident *uuid.UUID
		switch kind {
		case machine.KindOpened:
			incident = incidentID
			p.IncidentID, p.Reason, p.StartedAt = incidentID, in.Result.ErrorKind, &startedAt
		case machine.KindResolved:
			incident = incidentID
			resolved := in.Result.CheckedAt.UTC()
			p.IncidentID, p.StartedAt, p.ResolvedAt = incidentID, &startedAt, &resolved
			p.DurationSeconds = int64(resolved.Sub(startedAt).Seconds())
		case machine.KindTLSExpiring:
			na := in.Result.TLSNotAfter.UTC()
			days := int(na.Sub(in.Now).Hours() / 24)
			p.TLSNotAfter, p.DaysLeft = &na, &days
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("incidents: payload: %w", err)
		}
		if err := q.InsertDelivery(ctx, incidentsdb.InsertDeliveryParams{
			ID: uuidv7.New(), TenantID: in.TenantID, Kind: string(kind), MonitorID: in.MonitorID,
			IncidentID: incident, ChannelID: channel, Payload: raw,
		}); err != nil {
			return fmt.Errorf("incidents: enqueue %s: %w", kind, err)
		}
	}
	return nil
}

// ResolveOpen resolves the monitor's open incident, if any, with reason (ReasonPaused or
// ReasonDeleted) and without a notification (ADR-0016). tx must be a tenant transaction of
// tenantID; it is called by the monitors module on pause and delete.
func (s *Service) ResolveOpen(ctx context.Context, tx pgx.Tx, tenantID, monitorID uuid.UUID, reason string) error {
	q := incidentsdb.New(tx)
	st, err := q.LockMonitorState(ctx, incidentsdb.LockMonitorStateParams{TenantID: tenantID, MonitorID: monitorID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMonitorNotFound
	}
	if err != nil {
		return fmt.Errorf("incidents: lock monitor state: %w", err)
	}
	if st.CurrentIncidentID == nil {
		return nil
	}
	_, err = q.ResolveIncident(ctx, incidentsdb.ResolveIncidentParams{TenantID: tenantID, ID: *st.CurrentIncidentID, ResolveReason: &reason})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("incidents: resolve %s: %w", reason, err)
	}
	if err := q.ClearCurrentIncident(ctx, incidentsdb.ClearCurrentIncidentParams{TenantID: tenantID, MonitorID: monitorID}); err != nil {
		return fmt.Errorf("incidents: clear current incident: %w", err)
	}
	return nil
}

// payloadBase and payload are the outbox payload: what the Slack message (B-13) needs and no
// secret. The URL is reduced to scheme, host and path because query strings often carry tokens.
type payloadBase struct {
	MonitorID   uuid.UUID `json:"monitorId"`
	MonitorName string    `json:"monitorName"`
	URL         string    `json:"url"`
}

type payload struct {
	payloadBase
	IncidentID      *uuid.UUID `json:"incidentId,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	ResolvedAt      *time.Time `json:"resolvedAt,omitempty"`
	DurationSeconds int64      `json:"durationSeconds,omitempty"`
	TLSNotAfter     *time.Time `json:"tlsNotAfter,omitempty"`
	DaysLeft        *int       `json:"daysLeft,omitempty"`
}

func displayURL(config []byte) string {
	var c struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(config, &c) != nil {
		return ""
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
