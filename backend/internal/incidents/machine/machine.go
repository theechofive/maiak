// Package machine is the incident state machine (ADR-0007, ADR-0016): given the previous state
// of a monitor and of the location that produced a check result, it computes the new state,
// whether an incident opens or resolves, the outbox kinds to emit and the TLS expiry warning.
// It is pure (no database, no clock of its own) so every rule is covered by table tests;
// internal/incidents applies its output inside the result transaction.
package machine

import "time"

// Status is the status of a location or of the whole monitor.
type Status string

// Statuses.
const (
	Unknown Status = "unknown"
	Up      Status = "up"
	Down    Status = "down"
)

// Transition is the incident change caused by one result.
type Transition string

// Transitions.
const (
	None     Transition = "none"
	Opened   Transition = "opened"
	Resolved Transition = "resolved"
)

// Kind is a notification_deliveries kind.
type Kind string

// Outbox kinds.
const (
	KindOpened      Kind = "opened"
	KindResolved    Kind = "resolved"
	KindTLSExpiring Kind = "tls_expiring"
)

// TLSWarnWindow is how close to expiry a certificate must be to be warned about.
const TLSWarnWindow = 14 * 24 * time.Hour

// LocationState is the state of one (monitor, location).
type LocationState struct {
	Status              Status
	ConsecutiveFailures int
	// LastCheckedAt is the checked_at of the newest applied result; zero before the first.
	LastCheckedAt time.Time
}

// MonitorState is the aggregated state of a monitor.
type MonitorState struct {
	Status          Status
	StatusChangedAt time.Time
	LastErrorKind   string // empty: none
	LastCheckedAt   time.Time
	TLSExpiresAt    *time.Time
	// TLSWarnedNotAfter is the notAfter of the certificate already warned about.
	TLSWarnedNotAfter *time.Time
	IncidentOpen      bool
}

// Result is one check outcome.
type Result struct {
	OK          bool
	ErrorKind   string
	TLSNotAfter *time.Time
	CheckedAt   time.Time
}

// Config is the monitor configuration the machine needs.
type Config struct {
	// FailureThreshold is the number of consecutive failures that make a location down;
	// values below 1 count as 1.
	FailureThreshold int
	NotifySlack      bool
}

// Input is everything one step needs.
type Input struct {
	Monitor  MonitorState
	Location LocationState // the location that produced Result
	// Others are the monitor's other selected locations (empty in the MVP).
	Others []LocationState
	Result Result
	Config Config
	Now    time.Time
}

// Output is the new state and the side effects to record.
type Output struct {
	// Ignored is true for a result not newer than the location's last one; nothing changes.
	Ignored    bool
	Location   LocationState
	Monitor    MonitorState
	Transition Transition
	// Emit lists the outbox kinds to write, in order (empty when notifications are off).
	Emit []Kind
	// TLSWarning is true when a tls_expiring warning was decided for Result.TLSNotAfter; the
	// certificate counts as warned even when no notification is emitted.
	TLSWarning bool
}

// Apply computes one step.
func Apply(in Input) Output {
	out := Output{Location: in.Location, Monitor: in.Monitor, Transition: None}
	r := in.Result
	if !in.Location.LastCheckedAt.IsZero() && !r.CheckedAt.After(in.Location.LastCheckedAt) {
		out.Ignored = true
		return out
	}

	threshold := max(in.Config.FailureThreshold, 1)
	loc := &out.Location
	loc.LastCheckedAt = r.CheckedAt
	if r.OK {
		loc.Status, loc.ConsecutiveFailures = Up, 0
	} else {
		loc.ConsecutiveFailures++
		if loc.ConsecutiveFailures >= threshold {
			loc.Status = Down
		}
	}

	m := &out.Monitor
	if r.CheckedAt.After(m.LastCheckedAt) {
		m.LastCheckedAt = r.CheckedAt
	}
	if !r.OK {
		m.LastErrorKind = r.ErrorKind
	}
	if status := aggregate(*loc, in.Others); status != m.Status {
		m.Status, m.StatusChangedAt = status, r.CheckedAt
	}
	if m.Status == Up {
		m.LastErrorKind = ""
	}

	switch {
	case m.Status == Down && !m.IncidentOpen:
		m.IncidentOpen, out.Transition = true, Opened
		out.emit(in.Config, KindOpened)
	case m.Status == Up && m.IncidentOpen:
		m.IncidentOpen, out.Transition = false, Resolved
		out.emit(in.Config, KindResolved)
	}

	if na := r.TLSNotAfter; na != nil {
		v := *na
		m.TLSExpiresAt = &v
		if v.Sub(in.Now) <= TLSWarnWindow && (m.TLSWarnedNotAfter == nil || !m.TLSWarnedNotAfter.Equal(v)) {
			m.TLSWarnedNotAfter = &v
			out.TLSWarning = true
			out.emit(in.Config, KindTLSExpiring)
		}
	}
	return out
}

func (o *Output) emit(cfg Config, k Kind) {
	if cfg.NotifySlack {
		o.Emit = append(o.Emit, k)
	}
}

// aggregate is the MVP quorum rule: down when every selected location is down, up when any is
// up, unknown otherwise (no location has reported a definite state yet).
func aggregate(loc LocationState, others []LocationState) Status {
	allDown, anyUp := loc.Status == Down, loc.Status == Up
	for _, o := range others {
		allDown = allDown && o.Status == Down
		anyUp = anyUp || o.Status == Up
	}
	switch {
	case allDown:
		return Down
	case anyUp:
		return Up
	}
	return Unknown
}
