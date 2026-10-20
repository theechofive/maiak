package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/queries"
)

// Reads go through the dashboard read models (internal/queries); writes through the monitors
// service, after which the monitor is read back so every response has the same shape (uptime,
// ongoing incident, last response time).

func (s *server) actor(ctx context.Context) (monitors.Actor, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return monitors.Actor{}, err
	}
	return monitors.Actor{TenantID: p.TenantID, UserID: p.UserID, IP: httpx.ClientIP(httpRequest(ctx), s.cfg.TrustedProxyHops)}, nil
}

func (s *server) ListMonitors(ctx context.Context, _ gen.ListMonitorsRequestObject) (gen.ListMonitorsResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	list, sum, err := s.queries.ListMonitors(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	items := make([]gen.MonitorListItem, len(list))
	for i, m := range list {
		items[i] = gen.MonitorListItem{
			Id: m.ID, Name: m.Name, Url: m.URL, ExpectedStatus: m.ExpectedStatus,
			IntervalSeconds: m.IntervalSeconds, TimeoutSeconds: m.TimeoutSeconds,
			FailureThreshold: m.FailureThreshold, NotifySlack: m.NotifySlack, Paused: m.Paused,
			Status: gen.MonitorStatus(m.Status), StatusSince: m.StatusSince, LastErrorKind: m.LastErrorKind,
			LastCheckedAt: m.LastCheckedAt, TlsExpiresAt: m.TLSExpiresAt, OngoingIncident: toOngoing(m.Ongoing),
			Uptime24h: m.Uptime24h, LastResponseMs: m.LastResponseMs,
		}
	}
	return gen.ListMonitors200JSONResponse{
		Items:   items,
		Summary: gen.MonitorSummary{Up: sum.Up, Down: sum.Down, Paused: sum.Paused, Unknown: sum.Unknown},
	}, nil
}

func (s *server) CreateMonitor(ctx context.Context, req gen.CreateMonitorRequestObject) (gen.CreateMonitorResponseObject, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	m, err := s.monitors.Create(ctx, a, monitors.CreateInput{
		Name: b.Name, URL: b.Url, IntervalSeconds: b.IntervalSeconds, TimeoutSeconds: b.TimeoutSeconds,
		ExpectedStatus: b.ExpectedStatus, FailureThreshold: b.FailureThreshold, NotifySlack: b.NotifySlack,
	})
	if err != nil {
		return nil, err
	}
	d, err := s.detail(ctx, a.TenantID, m.ID)
	if err != nil {
		return nil, err
	}
	return gen.CreateMonitor201JSONResponse(d), nil
}

func (s *server) GetMonitor(ctx context.Context, req gen.GetMonitorRequestObject) (gen.GetMonitorResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.detail(ctx, p.TenantID, req.MonitorId)
	if err != nil {
		return nil, err
	}
	return gen.GetMonitor200JSONResponse(d), nil
}

func (s *server) UpdateMonitor(ctx context.Context, req gen.UpdateMonitorRequestObject) (gen.UpdateMonitorResponseObject, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	if _, err := s.monitors.Update(ctx, a, req.MonitorId, monitors.UpdateInput{
		Name: b.Name, URL: b.Url, IntervalSeconds: b.IntervalSeconds, TimeoutSeconds: b.TimeoutSeconds,
		ExpectedStatus: b.ExpectedStatus, FailureThreshold: b.FailureThreshold, NotifySlack: b.NotifySlack,
	}); err != nil {
		return nil, err
	}
	d, err := s.detail(ctx, a.TenantID, req.MonitorId)
	if err != nil {
		return nil, err
	}
	return gen.UpdateMonitor200JSONResponse(d), nil
}

func (s *server) DeleteMonitor(ctx context.Context, req gen.DeleteMonitorRequestObject) (gen.DeleteMonitorResponseObject, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.monitors.Delete(ctx, a, req.MonitorId); err != nil {
		return nil, err
	}
	return gen.DeleteMonitor204Response{}, nil
}

func (s *server) PauseMonitor(ctx context.Context, req gen.PauseMonitorRequestObject) (gen.PauseMonitorResponseObject, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.monitors.Pause(ctx, a, req.MonitorId); err != nil {
		return nil, err
	}
	d, err := s.detail(ctx, a.TenantID, req.MonitorId)
	if err != nil {
		return nil, err
	}
	return gen.PauseMonitor200JSONResponse(d), nil
}

func (s *server) ResumeMonitor(ctx context.Context, req gen.ResumeMonitorRequestObject) (gen.ResumeMonitorResponseObject, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.monitors.Resume(ctx, a, req.MonitorId); err != nil {
		return nil, err
	}
	d, err := s.detail(ctx, a.TenantID, req.MonitorId)
	if err != nil {
		return nil, err
	}
	return gen.ResumeMonitor200JSONResponse(d), nil
}

func (s *server) GetMonitorStats(ctx context.Context, req gen.GetMonitorStatsRequestObject) (gen.GetMonitorStatsResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	rng := gen.N24h
	if req.Params.Range != nil {
		rng = *req.Params.Range
	}
	if !queries.ValidRange(string(rng)) {
		return nil, &badRequestError{detail: "range must be 24h, 7d or 30d"}
	}
	st, err := s.queries.Stats(ctx, p.TenantID, req.MonitorId, string(rng))
	if err != nil {
		return nil, err
	}
	series := make([]gen.LatencyPoint, len(st.Series))
	for i, pt := range st.Series {
		series[i] = gen.LatencyPoint{T: pt.T, AvgMs: pt.AvgMs, OkRatio: pt.OKRatio}
	}
	return gen.GetMonitorStats200JSONResponse{Range: rng, AvgMs: st.AvgMs, P95Ms: st.P95Ms, Series: series}, nil
}

func (s *server) ListMonitorChecks(ctx context.Context, req gen.ListMonitorChecksRequestObject) (gen.ListMonitorChecksResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	limit := queries.DefaultCheckLimit
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	list, err := s.queries.Checks(ctx, p.TenantID, req.MonitorId, limit)
	if err != nil {
		return nil, err
	}
	items := make([]gen.Check, len(list))
	for i, c := range list {
		items[i] = gen.Check{CheckedAt: c.CheckedAt, Location: c.Location, Ok: c.OK, HttpStatus: c.HTTPStatus,
			ResponseMs: c.ResponseMs, ErrorKind: c.ErrorKind}
	}
	return gen.ListMonitorChecks200JSONResponse{Items: items}, nil
}

func (s *server) ListMonitorIncidents(ctx context.Context, req gen.ListMonitorIncidentsRequestObject) (gen.ListMonitorIncidentsResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.queries.Incidents(ctx, p.TenantID, req.MonitorId)
	if err != nil {
		return nil, err
	}
	items := make([]gen.Incident, len(list))
	for i, in := range list {
		items[i] = gen.Incident{Id: in.ID, StartedAt: in.StartedAt, ResolvedAt: in.ResolvedAt, Reason: in.Reason}
	}
	return gen.ListMonitorIncidents200JSONResponse{Items: items}, nil
}

func (s *server) detail(ctx context.Context, tenantID, id uuid.UUID) (gen.MonitorDetail, error) {
	d, err := s.queries.Monitor(ctx, tenantID, id)
	if err != nil {
		return gen.MonitorDetail{}, err
	}
	m := d.Monitor
	return gen.MonitorDetail{
		Id: m.ID, Name: m.Name, Url: m.URL, ExpectedStatus: m.ExpectedStatus,
		IntervalSeconds: m.IntervalSeconds, TimeoutSeconds: m.TimeoutSeconds,
		FailureThreshold: m.FailureThreshold, NotifySlack: m.NotifySlack, Paused: m.Paused,
		Status: gen.MonitorStatus(m.Status), StatusSince: m.StatusSince, LastErrorKind: m.LastErrorKind,
		LastCheckedAt: m.LastCheckedAt, TlsExpiresAt: m.TLSExpiresAt, OngoingIncident: toOngoing(m.Ongoing),
		Uptime:    gen.Uptime{H24: m.Uptime24h, D7: d.Uptime7d, D30: d.Uptime30d},
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}, nil
}

func toOngoing(in *queries.Incident) *gen.OngoingIncident {
	if in == nil {
		return nil
	}
	return &gen.OngoingIncident{Id: in.ID, StartedAt: in.StartedAt, Reason: in.Reason}
}
