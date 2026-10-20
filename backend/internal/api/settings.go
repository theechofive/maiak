package api

import (
	"context"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
)

// The Slack webhook URL is write-only: these handlers never return it, and errors from sending
// to it reach the client only as a kind (errors.go).

func (s *server) GetSlackSettings(ctx context.Context, _ gen.GetSlackSettingsRequestObject) (gen.GetSlackSettingsResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.slack.Get(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	return gen.GetSlackSettings200JSONResponse(toSlackSettings(st)), nil
}

func (s *server) PutSlackSettings(ctx context.Context, req gen.PutSlackSettingsRequestObject) (gen.PutSlackSettingsResponseObject, error) {
	a, err := s.notifyActor(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.slack.Put(ctx, a, req.Body.WebhookUrl)
	if err != nil {
		return nil, err
	}
	return gen.PutSlackSettings200JSONResponse(toSlackSettings(st)), nil
}

func (s *server) TestSlack(ctx context.Context, _ gen.TestSlackRequestObject) (gen.TestSlackResponseObject, error) {
	a, err := s.notifyActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.slack.Test(ctx, a); err != nil {
		return nil, err
	}
	return gen.TestSlack204Response{}, nil
}

func (s *server) notifyActor(ctx context.Context) (notify.Actor, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return notify.Actor{}, err
	}
	return notify.Actor{TenantID: p.TenantID, UserID: p.UserID, IP: httpx.ClientIP(httpRequest(ctx), s.cfg.TrustedProxyHops)}, nil
}

func toSlackSettings(st notify.Settings) gen.SlackSettings {
	out := gen.SlackSettings{Configured: st.Configured}
	if st.Configured {
		status := gen.SlackSettingsStatus(st.Status)
		out.Status = &status
	}
	return out
}
