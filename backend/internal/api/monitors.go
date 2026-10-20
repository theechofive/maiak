package api

import (
	"context"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
)

// Uptime, the last response time and the ongoing incident come from results, rollups and
// incidents, which arrive with B-10, B-11 and B-15; until then they are null.

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
	list, sum, err := s.monitors.List(ctx, p.TenantID)
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
			LastCheckedAt: m.LastCheckedAt, TlsExpiresAt: m.TLSExpiresAt,
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
	return gen.CreateMonitor201JSONResponse(toMonitorDetail(m)), nil
}

func (s *server) GetMonitor(ctx context.Context, req gen.GetMonitorRequestObject) (gen.GetMonitorResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	m, err := s.monitors.Get(ctx, p.TenantID, req.MonitorId)
	if err != nil {
		return nil, err
	}
	return gen.GetMonitor200JSONResponse(toMonitorDetail(m)), nil
}

func (s *server) UpdateMonitor(ctx context.Context, req gen.UpdateMonitorRequestObject) (gen.UpdateMonitorResponseObject, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	m, err := s.monitors.Update(ctx, a, req.MonitorId, monitors.UpdateInput{
		Name: b.Name, URL: b.Url, IntervalSeconds: b.IntervalSeconds, TimeoutSeconds: b.TimeoutSeconds,
		ExpectedStatus: b.ExpectedStatus, FailureThreshold: b.FailureThreshold, NotifySlack: b.NotifySlack,
	})
	if err != nil {
		return nil, err
	}
	return gen.UpdateMonitor200JSONResponse(toMonitorDetail(m)), nil
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
	m, err := s.monitors.Pause(ctx, a, req.MonitorId)
	if err != nil {
		return nil, err
	}
	return gen.PauseMonitor200JSONResponse(toMonitorDetail(m)), nil
}

func (s *server) ResumeMonitor(ctx context.Context, req gen.ResumeMonitorRequestObject) (gen.ResumeMonitorResponseObject, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	m, err := s.monitors.Resume(ctx, a, req.MonitorId)
	if err != nil {
		return nil, err
	}
	return gen.ResumeMonitor200JSONResponse(toMonitorDetail(m)), nil
}

func toMonitorDetail(m monitors.Monitor) gen.MonitorDetail {
	return gen.MonitorDetail{
		Id: m.ID, Name: m.Name, Url: m.URL, ExpectedStatus: m.ExpectedStatus,
		IntervalSeconds: m.IntervalSeconds, TimeoutSeconds: m.TimeoutSeconds,
		FailureThreshold: m.FailureThreshold, NotifySlack: m.NotifySlack, Paused: m.Paused,
		Status: gen.MonitorStatus(m.Status), StatusSince: m.StatusSince, LastErrorKind: m.LastErrorKind,
		LastCheckedAt: m.LastCheckedAt, TlsExpiresAt: m.TLSExpiresAt,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}
