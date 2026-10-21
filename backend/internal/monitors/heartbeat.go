package monitors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/incidents/machine"
	"github.com/theechofive/maiak/backend/internal/monitors/monitorsdb"
	"github.com/theechofive/maiak/backend/internal/platform/db"
)

// ErrorKindHeartbeatMissed is the error kind of a missed heartbeat deadline (monitor state,
// incident reason, Slack message).
const ErrorKindHeartbeatMissed = "heartbeat_missed"

// HeartbeatPath is the path of the public ping endpoint; a heartbeat's URL is MAIAK_PUBLIC_URL,
// this path and the token.
const HeartbeatPath = "/api/v1/heartbeat/"

// ErrUnknownHeartbeat means no heartbeat has that token. It is the only failure a ping reports,
// whatever the reason (malformed, never issued, regenerated).
var ErrUnknownHeartbeat = errors.New("monitors: unknown heartbeat token")

const heartbeatTokenBytes = 32

// NewHeartbeatToken returns a random 32-byte token, URL-safe base64 without padding, and the
// SHA-256 hash that is stored instead of it. A slow hash is unnecessary: the token has 256 bits.
func NewHeartbeatToken() (string, []byte, error) {
	b := make([]byte, heartbeatTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("monitors: random heartbeat token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	return token, HashHeartbeatToken(token), nil
}

// HashHeartbeatToken returns the SHA-256 of a token as given in the URL. Any string is hashed,
// valid or not, so an unknown token costs the same as a known one up to the lookup.
func HashHeartbeatToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// PingResult is the outcome of an accepted ping of a known token.
type PingResult struct {
	TenantID  uuid.UUID
	MonitorID uuid.UUID
	// Recorded is false when the monitor is paused or deleted, or its tenant is not active: the
	// ping is answered like any other but changes nothing.
	Recorded bool
}

// Ping records a ping for the token whose hash (HashHeartbeatToken) is given, through the SECURITY DEFINER function heartbeat_ping, which
// finds the heartbeat by the token's hash before the tenant is known. It returns
// ErrUnknownHeartbeat for any token that does not belong to a heartbeat. The incident machine
// sees the ping later, in the jobs evaluation (ADR-0094).
func (s *Service) Ping(ctx context.Context, hash []byte) (PingResult, error) {
	var res PingResult
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id, monitor_id, recorded FROM heartbeat_ping($1)`, hash).
			Scan(&res.TenantID, &res.MonitorID, &res.Recorded)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PingResult{}, ErrUnknownHeartbeat
	}
	if err != nil {
		return PingResult{}, fmt.Errorf("monitors: ping: %w", err)
	}
	return res, nil
}

// RegenerateHeartbeatToken gives a heartbeat a new token and returns it; the old URL stops
// working at commit. The token is shown once and never stored.
func (s *Service) RegenerateHeartbeatToken(ctx context.Context, a Actor, id uuid.UUID) (string, error) {
	token, hash, err := NewHeartbeatToken()
	if err != nil {
		return "", err
	}
	err = db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		q := monitorsdb.New(tx)
		cur, err := q.LockMonitor(ctx, monitorsdb.LockMonitorParams{TenantID: a.TenantID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock monitor: %w", err)
		}
		if cur.Type != TypeHeartbeat {
			return ErrNotHeartbeat
		}
		n, err := q.ReplaceHeartbeatToken(ctx, monitorsdb.ReplaceHeartbeatTokenParams{TenantID: a.TenantID, MonitorID: id, TokenHash: hash})
		if err != nil {
			return fmt.Errorf("replace heartbeat token: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("replace heartbeat token: %d rows", n)
		}
		return s.audit(ctx, tx, a, ActionTokenRegenerated, id, nil)
	})
	if err != nil {
		return "", wrap("regenerate heartbeat token", err)
	}
	return token, nil
}

// lockHeartbeat takes the heartbeat row lock that serialises every change of a heartbeat's state.
func lockHeartbeat(ctx context.Context, q *monitorsdb.Queries, tenantID, id uuid.UUID) error {
	if _, err := q.LockHeartbeat(ctx, monitorsdb.LockHeartbeatParams{TenantID: tenantID, MonitorID: id}); err != nil {
		return fmt.Errorf("lock heartbeat: %w", err)
	}
	return nil
}

// HeartbeatDeadline is when a heartbeat becomes overdue: the newer of the start of the wait
// (creation or resume) and the last ping, plus the expected interval and the grace. A ping from
// before the wait started (before a resume) does not count.
func HeartbeatDeadline(waitingSince time.Time, lastPing *time.Time, interval, grace time.Duration) time.Time {
	base := waitingSince
	if lastPing != nil && lastPing.After(base) {
		base = *lastPing
	}
	return base.Add(interval + grace)
}

// heartbeatState is what one evaluation of a heartbeat reads under its row lock.
type heartbeatState struct {
	Paused         bool
	WaitingSince   time.Time
	LastPing       *time.Time
	AppliedPing    *time.Time
	MissedDeadline *time.Time
	Interval       time.Duration
	Grace          time.Duration
}

// heartbeatStep is what an evaluation applies to the incident machine, in this order: an OK
// result at the time of a ping the machine has not seen, then a failed result at a deadline
// that passed and was not applied yet. Nil means nothing to apply.
type heartbeatStep struct {
	Ping   *time.Time
	Missed *time.Time
}

// nextHeartbeatStep decides one evaluation at now. A paused heartbeat has nothing to apply; a
// deadline is missed only once now is strictly after it, and only once per deadline.
func nextHeartbeatStep(st heartbeatState, now time.Time) heartbeatStep {
	var step heartbeatStep
	if st.Paused {
		return step
	}
	if st.LastPing != nil && (st.AppliedPing == nil || st.LastPing.After(*st.AppliedPing)) {
		p := *st.LastPing
		step.Ping = &p
	}
	deadline := HeartbeatDeadline(st.WaitingSince, st.LastPing, st.Interval, st.Grace)
	if now.After(deadline) && (st.MissedDeadline == nil || st.MissedDeadline.Before(deadline)) {
		step.Missed = &deadline
	}
	return step
}

// MaxHeartbeatsPerRun bounds one evaluation run; the rest waits for the next run.
const MaxHeartbeatsPerRun = 500

// EvaluateHeartbeats is the jobs task of ADR-0094: it lists the heartbeats with an unseen ping
// or an unapplied missed deadline (list_due_heartbeats, a SECURITY DEFINER function) and applies
// each in its own tenant transaction through the incident machine. A missed deadline is a failed
// result with error kind heartbeat_missed at the deadline (failure threshold 1: the monitor is
// down and an incident opens, with an outbox row when Slack is on); a ping is an OK result at the
// ping time, which resolves an open incident. It returns how many missed deadlines it applied.
// The caller must run it as a singleton (the housekeeping advisory lock).
func (s *Service) EvaluateHeartbeats(ctx context.Context) (int, error) {
	type due struct{ tenantID, monitorID uuid.UUID }
	var list []due
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, monitor_id FROM list_due_heartbeats($1)`, MaxHeartbeatsPerRun)
		if err != nil {
			return err
		}
		list, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
			var d due
			return d, r.Scan(&d.tenantID, &d.monitorID)
		})
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("monitors: list due heartbeats: %w", err)
	}
	missed := 0
	var errs []error
	for _, d := range list {
		ok, err := s.evaluateHeartbeat(ctx, d.tenantID, d.monitorID)
		if err != nil {
			errs = append(errs, fmt.Errorf("heartbeat %s: %w", d.monitorID, err))
			continue
		}
		if ok {
			missed++
		}
	}
	return missed, errors.Join(errs...)
}

// evaluateHeartbeat applies what is due for one heartbeat and reports whether a missed deadline
// changed the monitor's state.
func (s *Service) evaluateHeartbeat(ctx context.Context, tenantID, id uuid.UUID) (bool, error) {
	missed := false
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := monitorsdb.New(tx)
		hb, err := q.LockHeartbeat(ctx, monitorsdb.LockHeartbeatParams{TenantID: tenantID, MonitorID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock heartbeat: %w", err)
		}
		m, err := q.GetMonitor(ctx, monitorsdb.GetMonitorParams{TenantID: tenantID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted since it was listed
		}
		if err != nil {
			return fmt.Errorf("get monitor: %w", err)
		}
		var cfg heartbeatConfig
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			return fmt.Errorf("monitor config: %w", err)
		}
		// The database clock, read after the row lock, so a ping committed before it counts.
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return fmt.Errorf("clock: %w", err)
		}
		step := nextHeartbeatStep(heartbeatState{
			Paused: m.Paused, WaitingSince: hb.WaitingSince, LastPing: hb.LastPingAt, AppliedPing: hb.AppliedPingAt,
			MissedDeadline: hb.MissedDeadline, Interval: time.Duration(m.IntervalSeconds) * time.Second,
			Grace: time.Duration(cfg.GraceSeconds) * time.Second,
		}, now)
		if step.Ping == nil && step.Missed == nil {
			return nil
		}
		locs, err := s.sched.LocationStates(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if len(locs) == 0 {
			return fmt.Errorf("heartbeat has no location state")
		}
		in := incidents.ResultInput{TenantID: tenantID, MonitorID: id, LocationID: locs[0].LocationID, Now: now}
		applied, missedAt := hb.AppliedPingAt, hb.MissedDeadline
		if step.Ping != nil {
			// An OK result changes nothing for a heartbeat that is already up (one location,
			// failure threshold 1), so the ping is only marked as seen (ADR-0096): steady pings
			// cost one row update here, not the incident machine's writes.
			if locs[0].Status != string(machine.Up) {
				in.Result = machine.Result{OK: true, CheckedAt: *step.Ping}
				if _, err := s.incidents.ApplyResult(ctx, tx, in); err != nil {
					return err
				}
			}
			applied = step.Ping
		}
		if step.Missed != nil {
			in.Result = machine.Result{ErrorKind: ErrorKindHeartbeatMissed, CheckedAt: *step.Missed}
			out, err := s.incidents.ApplyResult(ctx, tx, in)
			if err != nil {
				return err
			}
			missedAt, missed = step.Missed, !out.Ignored
		}
		return q.MarkHeartbeatApplied(ctx, monitorsdb.MarkHeartbeatAppliedParams{
			TenantID: tenantID, MonitorID: id, AppliedPingAt: applied, MissedDeadline: missedAt,
		})
	})
	if err != nil {
		return false, err
	}
	return missed, nil
}
