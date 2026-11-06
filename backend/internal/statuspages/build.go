package statuspages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/statuspages/statuspagesdb"
)

// Build computes the public snapshot of a page inside a tenant transaction. It is the only code
// that decides what becomes public (ADR-0103): public names and groups, states, daily bars and
// generated incident titles. Monitor names, URLs, projects, error kinds and ids never enter it.
func Build(ctx context.Context, tx pgx.Tx, tenantID, pageID uuid.UUID, now time.Time) (Snapshot, error) {
	q := statuspagesdb.New(tx)
	page, err := q.GetPageForBuild(ctx, statuspagesdb.GetPageForBuildParams{TenantID: tenantID, ID: pageID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("statuspages: page: %w", err)
	}
	return build(ctx, q, tenantID, page, now.UTC())
}

func build(ctx context.Context, q *statuspagesdb.Queries, tenantID uuid.UUID, page statuspagesdb.GetPageForBuildRow, now time.Time) (Snapshot, error) {
	comps, err := q.ListComponents(ctx, statuspagesdb.ListComponentsParams{TenantID: tenantID, PageID: page.ID})
	if err != nil {
		return Snapshot{}, fmt.Errorf("statuspages: components: %w", err)
	}
	days := HistoryWindow(now)
	since := days[0]
	ids := make([]uuid.UUID, len(comps))
	for i, c := range comps {
		ids[i] = c.MonitorID
	}

	counts := map[uuid.UUID]map[string]Counts{}
	if len(ids) > 0 {
		rows, err := q.DailyCounts(ctx, statuspagesdb.DailyCountsParams{TenantID: tenantID, MonitorIds: ids, Since: since})
		if err != nil {
			return Snapshot{}, fmt.Errorf("statuspages: daily counts: %w", err)
		}
		for _, r := range rows {
			if counts[r.MonitorID] == nil {
				counts[r.MonitorID] = map[string]Counts{}
			}
			counts[r.MonitorID][r.Day] = Counts{Total: r.Total, OK: r.Ok}
		}
	}
	var incidents []statuspagesdb.RecentIncidentsRow
	if len(ids) > 0 {
		if incidents, err = q.RecentIncidents(ctx, statuspagesdb.RecentIncidentsParams{TenantID: tenantID, MonitorIds: ids, Since: &since}); err != nil {
			return Snapshot{}, fmt.Errorf("statuspages: incidents: %w", err)
		}
	}
	byMonitor := map[uuid.UUID][]Interval{}
	for _, inc := range incidents {
		if inc.MonitorID != nil {
			byMonitor[*inc.MonitorID] = append(byMonitor[*inc.MonitorID], Interval{Start: inc.StartedAt, End: inc.ResolvedAt})
		}
	}

	snap := Snapshot{
		Version:     SnapshotVersion,
		Page:        PublicPage{Slug: page.Slug, Title: page.Title, Description: page.Description, LogoURL: page.LogoUrl, AccentColor: page.AccentColor},
		GeneratedAt: now.Truncate(time.Second),
		Components:  make([]Component, 0, len(comps)),
		// Empty lists, not null, so the JSON shape never changes.
		ActiveIncidents: []Incident{},
		PastIncidents:   []Incident{},
	}
	names := map[uuid.UUID]string{}
	states := make([]Status, 0, len(comps))
	for _, c := range comps {
		names[c.MonitorID] = c.PublicName
		st := ComponentStatus(MonitorState{Paused: c.Paused, Status: c.MonitorStatus,
			FailingLocations: int(c.FailingLocations), DownLocations: int(c.DownLocations)})
		var bars []Day
		var uptime *float64
		if c.MonitorType == "heartbeat" {
			bars, uptime = HeartbeatDays(days, c.MonitorCreatedAt, now, byMonitor[c.MonitorID])
		} else {
			bars, uptime = CheckDays(days, counts[c.MonitorID])
		}
		snap.Components = append(snap.Components, Component{Name: c.PublicName, Group: c.GroupName, Status: st, Uptime: uptime, Days: bars})
		states = append(states, st)
	}
	snap.Status = OverallStatus(states)

	for _, inc := range incidents {
		if inc.MonitorID == nil {
			continue
		}
		name, ok := names[*inc.MonitorID]
		if !ok {
			continue
		}
		pub := Incident{
			ID:         IncidentRef(page.ID, inc.ID),
			Title:      IncidentTitle([]string{name}),
			Components: []string{name},
			StartedAt:  inc.StartedAt.UTC().Truncate(time.Second),
		}
		end := now
		if inc.ResolvedAt != nil {
			r := inc.ResolvedAt.UTC().Truncate(time.Second)
			pub.ResolvedAt = &r
			end = r
		}
		pub.DurationSeconds = max(int64(end.Sub(pub.StartedAt)/time.Second), 0)
		if pub.ResolvedAt == nil {
			snap.ActiveIncidents = append(snap.ActiveIncidents, pub)
		} else {
			snap.PastIncidents = append(snap.PastIncidents, pub)
		}
	}
	// Every open incident is listed; past ones only the newest MaxPastIncidents (the bars above
	// used all of them).
	if len(snap.PastIncidents) > MaxPastIncidents {
		snap.PastIncidents = snap.PastIncidents[:MaxPastIncidents]
	}
	return snap, nil
}

// MaxPastIncidents is how many resolved incidents a snapshot lists.
const MaxPastIncidents = 100

// RefreshForMonitor refreshes, in the caller's tenant transaction, every published page that shows
// the monitor. monitors calls it when it pauses, resumes or deletes one, so the public page
// changes with the next cache miss instead of the next 30 s rebuild.
func RefreshForMonitor(ctx context.Context, tx pgx.Tx, tenantID, monitorID uuid.UUID, now time.Time) error {
	ids, err := statuspagesdb.New(tx).PublishedPagesWithMonitor(ctx, statuspagesdb.PublishedPagesWithMonitorParams{TenantID: tenantID, MonitorID: monitorID})
	if err != nil {
		return fmt.Errorf("statuspages: pages of monitor: %w", err)
	}
	for _, id := range ids {
		if _, err := Refresh(ctx, tx, tenantID, id, now); err != nil {
			return err
		}
	}
	return nil
}

// Refresh rebuilds and stores the snapshot of a page in the caller's tenant transaction, or
// removes it when the page is not published (or gone). It returns whether a snapshot is stored.
func Refresh(ctx context.Context, tx pgx.Tx, tenantID, pageID uuid.UUID, now time.Time) (bool, error) {
	q := statuspagesdb.New(tx)
	page, err := q.GetPageForBuild(ctx, statuspagesdb.GetPageForBuildParams{TenantID: tenantID, ID: pageID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !page.Published) {
		if err := q.DeleteSnapshot(ctx, statuspagesdb.DeleteSnapshotParams{TenantID: tenantID, PageID: pageID}); err != nil {
			return false, fmt.Errorf("statuspages: delete snapshot: %w", err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("statuspages: page: %w", err)
	}
	snap, err := build(ctx, q, tenantID, page, now.UTC())
	if err != nil {
		return false, err
	}
	payload, etag, err := snap.Encode()
	if err != nil {
		return false, fmt.Errorf("statuspages: encode: %w", err)
	}
	if err := q.UpsertSnapshot(ctx, statuspagesdb.UpsertSnapshotParams{
		PageID: pageID, TenantID: tenantID, Slug: page.Slug, Payload: payload, Etag: etag, GeneratedAt: snap.GeneratedAt,
	}); err != nil {
		return false, fmt.Errorf("statuspages: store snapshot: %w", err)
	}
	return true, nil
}
