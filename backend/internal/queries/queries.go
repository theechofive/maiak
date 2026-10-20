// Package queries holds the read models of the dashboard: monitor list and detail, statistics,
// recent checks and incidents. They read across tables owned by monitors, scheduler, results
// and incidents but never write (CLAUDE.md rule 1). Every query runs in a tenant transaction
// (db.WithTenantTx) and reads raw results only through the (monitor_id, checked_at) key, for
// bounded windows; longer ranges come from the hourly rollups (ADR-0005, ADR-0016, ADR-0041).
package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/db"
)

// ErrNotFound means no live monitor with that id exists in the tenant.
var ErrNotFound = errors.New("queries: monitor not found")

// Monitor statuses as reported to clients.
const (
	StatusUnknown = "unknown"
	StatusUp      = "up"
	StatusDown    = "down"
)

// Limits of the list endpoints.
const (
	DefaultCheckLimit = 50
	MaxCheckLimit     = 200
	MaxIncidents      = 100
)

// unknownReason stands in for an incident without a recorded error kind.
const unknownReason = "unknown"

// Service answers the dashboard's read requests.
type Service struct {
	pool  *db.Pool
	clock clock.Clock
}

// New returns a Service; c defaults to the system clock.
func New(pool *db.Pool, c clock.Clock) *Service {
	if c == nil {
		c = clock.System{}
	}
	return &Service{pool: pool, clock: c}
}

// Incident is one incident of a monitor; ResolvedAt is nil while it is ongoing.
type Incident struct {
	ID         uuid.UUID
	StartedAt  time.Time
	ResolvedAt *time.Time
	Reason     string
}

// Monitor is a monitor with its current state, as the list shows it.
type Monitor struct {
	ID               uuid.UUID
	Name             string
	URL              string
	ExpectedStatus   string
	IntervalSeconds  int
	TimeoutSeconds   int
	FailureThreshold int
	NotifySlack      bool
	Paused           bool
	// Status is "unknown" for a paused monitor (it is not being checked).
	Status        string
	StatusSince   time.Time
	LastErrorKind *string
	LastCheckedAt *time.Time
	TLSExpiresAt  *time.Time
	// Ongoing is the open incident, nil when there is none.
	Ongoing *Incident
	// Uptime24h is the percentage of successful checks in the last 24 h; nil when none ran.
	Uptime24h *float64
	// LastResponseMs is the latency of the newest check; nil when none ran or it got no response.
	LastResponseMs *int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Detail is one monitor with uptime over 24 h, 7 d and 30 d.
type Detail struct {
	Monitor
	Uptime7d  *float64
	Uptime30d *float64
}

// Summary counts monitors by display state; a paused monitor counts only as paused.
type Summary struct {
	Up, Down, Paused, Unknown int
}

const monitorColumns = `
SELECT m.id, m.name, COALESCE(m.config ->> 'url', ''), COALESCE(m.config ->> 'expectedStatus', '2xx'),
       m.interval_seconds, m.timeout_seconds, m.failure_threshold, m.notify_slack, m.paused,
       m.created_at, m.updated_at,
       s.status, s.status_changed_at, s.last_error_kind, s.last_checked_at, s.tls_expires_at,
       i.id, i.started_at, COALESCE(i.reason, '` + unknownReason + `'),
       lr.latency_ms
FROM monitors AS m
JOIN monitor_state AS s ON s.tenant_id = m.tenant_id AND s.monitor_id = m.id
LEFT JOIN incidents AS i ON i.tenant_id = s.tenant_id AND i.id = s.current_incident_id AND i.resolved_at IS NULL
-- The newest result through the primary key (monitor_id, checked_at); the lower bound lets the
-- planner prune old monthly partitions.
LEFT JOIN LATERAL (
    SELECT c.latency_ms
    FROM check_results AS c
    WHERE c.monitor_id = m.id AND s.last_checked_at IS NOT NULL
      AND c.checked_at >= s.last_checked_at - interval '1 hour'
    ORDER BY c.checked_at DESC
    LIMIT 1
) AS lr ON true
WHERE m.tenant_id = $1 AND m.deleted_at IS NULL`

func scanMonitor(row pgx.Row) (Monitor, error) {
	var m Monitor
	var interval, timeout, threshold int32
	var incID *uuid.UUID
	var incStarted *time.Time
	var incReason string
	var latency *int32
	err := row.Scan(&m.ID, &m.Name, &m.URL, &m.ExpectedStatus, &interval, &timeout, &threshold, &m.NotifySlack, &m.Paused,
		&m.CreatedAt, &m.UpdatedAt, &m.Status, &m.StatusSince, &m.LastErrorKind, &m.LastCheckedAt, &m.TLSExpiresAt,
		&incID, &incStarted, &incReason, &latency)
	if err != nil {
		return Monitor{}, err
	}
	m.IntervalSeconds, m.TimeoutSeconds, m.FailureThreshold = int(interval), int(timeout), int(threshold)
	if m.Paused {
		m.Status = StatusUnknown
	}
	if incID != nil && incStarted != nil {
		m.Ongoing = &Incident{ID: *incID, StartedAt: *incStarted, Reason: incReason}
	}
	if latency != nil {
		v := int(*latency)
		m.LastResponseMs = &v
	}
	return m, nil
}

// ListMonitors returns the live monitors of the tenant, oldest first, with summary counts.
// Uptime comes from the hourly rollups plus the raw rows of the hours not rolled up yet.
func (s *Service) ListMonitors(ctx context.Context, tenantID uuid.UUID) ([]Monitor, Summary, error) {
	now := s.clock.Now()
	var out []Monitor
	var sum Summary
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, monitorColumns+` ORDER BY m.created_at, m.id`, tenantID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Monitor, error) { return scanMonitor(r) })
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, len(out))
		for i, m := range out {
			ids[i] = m.ID
		}
		up, err := uptime(ctx, tx, ids, now.Add(-24*time.Hour), now)
		if err != nil {
			return err
		}
		for i := range out {
			out[i].Uptime24h = up[out[i].ID]
			switch {
			case out[i].Paused:
				sum.Paused++
			case out[i].Status == StatusUp:
				sum.Up++
			case out[i].Status == StatusDown:
				sum.Down++
			default:
				sum.Unknown++
			}
		}
		return nil
	})
	if err != nil {
		return nil, Summary{}, fmt.Errorf("queries: list monitors: %w", err)
	}
	if out == nil {
		out = []Monitor{}
	}
	return out, sum, nil
}

// Monitor returns one live monitor with uptime over 24 h, 7 d and 30 d.
func (s *Service) Monitor(ctx context.Context, tenantID, id uuid.UUID) (Detail, error) {
	now := s.clock.Now()
	var d Detail
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		m, err := scanMonitor(tx.QueryRow(ctx, monitorColumns+` AND m.id = $2`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		d.Monitor = m
		ids := []uuid.UUID{id}
		for _, w := range []struct {
			span time.Duration
			dst  **float64
		}{{24 * time.Hour, &d.Uptime24h}, {7 * 24 * time.Hour, &d.Uptime7d}, {30 * 24 * time.Hour, &d.Uptime30d}} {
			up, err := uptime(ctx, tx, ids, now.Add(-w.span), now)
			if err != nil {
				return err
			}
			*w.dst = up[id]
		}
		return nil
	})
	if err != nil {
		return Detail{}, wrap("monitor", err)
	}
	return d, nil
}

// uptimeSQL counts checks of each monitor in [$2, $5): rollups for the whole hours
// [$3, $4) that are rolled up, raw rows for the partial first hour [$2, $3) and for the hours
// after the newest rollup (the current hour and any hour jobs has not rolled up yet).
const uptimeSQL = `
SELECT m.id,
       COALESCE(r.total, 0) + head.total + tail.total,
       COALESCE(r.ok, 0) + head.ok + tail.ok
FROM unnest($1::uuid[]) AS m(id)
CROSS JOIN LATERAL (
    SELECT COALESCE(max(h.hour) + interval '1 hour', $3::timestamptz) AS raw_from,
           sum(h.total) AS total, sum(h.ok_count) AS ok
    FROM result_rollups_hourly AS h
    WHERE h.monitor_id = m.id AND h.hour >= $3 AND h.hour < $4
) AS r
CROSS JOIN LATERAL (
    SELECT count(*) AS total, count(*) FILTER (WHERE c.ok) AS ok
    FROM check_results AS c
    WHERE c.monitor_id = m.id AND c.checked_at >= $2 AND c.checked_at < $3
) AS head
CROSS JOIN LATERAL (
    SELECT count(*) AS total, count(*) FILTER (WHERE c.ok) AS ok
    FROM check_results AS c
    WHERE c.monitor_id = m.id AND c.checked_at >= r.raw_from AND c.checked_at < $5
) AS tail`

// uptime returns, per monitor, the percentage of successful checks in [from, to), or nil when
// no check ran. Checks that ran are the only denominator, so paused time does not count.
func uptime(ctx context.Context, tx pgx.Tx, ids []uuid.UUID, from, to time.Time) (map[uuid.UUID]*float64, error) {
	out := make(map[uuid.UUID]*float64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	from, to = from.UTC(), to.UTC()
	firstHour := from.Truncate(time.Hour)
	if firstHour.Before(from) {
		firstHour = firstHour.Add(time.Hour)
	}
	rows, err := tx.Query(ctx, uptimeSQL, ids, from, firstHour, to.Truncate(time.Hour), to)
	if err != nil {
		return nil, fmt.Errorf("uptime: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var total, ok int64
		if err := rows.Scan(&id, &total, &ok); err != nil {
			return nil, fmt.Errorf("uptime: %w", err)
		}
		out[id] = ratio(ok, total, 100)
	}
	return out, rows.Err()
}

func ratio(ok, total int64, scale float64) *float64 {
	if total <= 0 {
		return nil
	}
	v := float64(ok) / float64(total) * scale
	return &v
}

// Check is one raw check result.
type Check struct {
	CheckedAt  time.Time
	Location   string
	OK         bool
	HTTPStatus *int
	ResponseMs *int
	ErrorKind  *string
}

// Checks returns the newest checks of a monitor, at most limit (1..MaxCheckLimit).
func (s *Service) Checks(ctx context.Context, tenantID, id uuid.UUID, limit int) ([]Check, error) {
	limit = min(max(limit, 1), MaxCheckLimit)
	var out []Check
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := exists(ctx, tx, tenantID, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT c.checked_at, l.code, c.ok, c.status_code, c.latency_ms, c.error_kind
			FROM check_results AS c
			JOIN probe_locations AS l ON l.id = c.location_id
			WHERE c.monitor_id = $1
			ORDER BY c.checked_at DESC, l.code
			LIMIT $2`, id, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Check, error) {
			var c Check
			var status *int16
			var latency *int32
			if err := r.Scan(&c.CheckedAt, &c.Location, &c.OK, &status, &latency, &c.ErrorKind); err != nil {
				return c, err
			}
			if status != nil {
				v := int(*status)
				c.HTTPStatus = &v
			}
			if latency != nil {
				v := int(*latency)
				c.ResponseMs = &v
			}
			return c, nil
		})
		return err
	})
	if err != nil {
		return nil, wrap("checks", err)
	}
	if out == nil {
		out = []Check{}
	}
	return out, nil
}

// Incidents returns the incidents of a monitor, newest first, at most MaxIncidents.
func (s *Service) Incidents(ctx context.Context, tenantID, id uuid.UUID) ([]Incident, error) {
	var out []Incident
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := exists(ctx, tx, tenantID, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT i.id, i.started_at, i.resolved_at, COALESCE(i.reason, '`+unknownReason+`')
			FROM incident_monitors AS im
			JOIN incidents AS i ON i.tenant_id = im.tenant_id AND i.id = im.incident_id
			WHERE im.tenant_id = $1 AND im.monitor_id = $2
			ORDER BY i.started_at DESC, i.id DESC
			LIMIT $3`, tenantID, id, MaxIncidents)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Incident, error) {
			var i Incident
			err := r.Scan(&i.ID, &i.StartedAt, &i.ResolvedAt, &i.Reason)
			return i, err
		})
		return err
	})
	if err != nil {
		return nil, wrap("incidents", err)
	}
	if out == nil {
		out = []Incident{}
	}
	return out, nil
}

func exists(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM monitors WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func wrap(op string, err error) error {
	if errors.Is(err, ErrNotFound) {
		return err
	}
	return fmt.Errorf("queries: %s: %w", op, err)
}
