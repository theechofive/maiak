package api

import (
	"context"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
)

// The webhook URL of a channel is write-only: these handlers never return it, and errors from
// sending to it reach the client only as a kind (errors.go).

func (s *server) ListChannels(ctx context.Context, req gen.ListChannelsRequestObject) (gen.ListChannelsResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	if req.Params.ProjectId != nil {
		// 404 for a project the caller does not see.
		if _, err := s.projectFilter(ctx, req.Params.ProjectId); err != nil {
			return nil, err
		}
	}
	list, err := s.slack.List(ctx, p.TenantID, sc, req.Params.ProjectId)
	if err != nil {
		return nil, err
	}
	items := make([]gen.Channel, len(list))
	for i, c := range list {
		items[i] = toChannel(c)
	}
	return gen.ListChannels200JSONResponse{Items: items}, nil
}

func (s *server) CreateChannel(ctx context.Context, req gen.CreateChannelRequestObject) (gen.CreateChannelResponseObject, error) {
	a, err := s.notifyActor(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	c, err := s.slack.Create(ctx, a, notify.CreateInput{
		Name: b.Name, WebhookURL: b.WebhookUrl, Events: fromEvents(b.Events), Mention: b.Mention, ReminderMinutes: b.ReminderMinutes,
		ProjectID: b.ProjectId, OrganizationWide: b.OrganizationWide != nil && *b.OrganizationWide,
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateChannel201JSONResponse(toChannel(c)), nil
}

func (s *server) GetChannel(ctx context.Context, req gen.GetChannelRequestObject) (gen.GetChannelResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.slack.Get(ctx, p.TenantID, sc, req.ChannelId)
	if err != nil {
		return nil, err
	}
	return gen.GetChannel200JSONResponse(toChannel(c)), nil
}

func (s *server) UpdateChannel(ctx context.Context, req gen.UpdateChannelRequestObject) (gen.UpdateChannelResponseObject, error) {
	a, err := s.notifyActor(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	c, err := s.slack.Update(ctx, a, req.ChannelId, notify.UpdateInput{
		Name: b.Name, WebhookURL: b.WebhookUrl, Events: fromEvents(b.Events), Mention: b.Mention, ReminderMinutes: b.ReminderMinutes,
		ProjectID: b.ProjectId, OrganizationWide: b.OrganizationWide != nil && *b.OrganizationWide,
	})
	if err != nil {
		return nil, err
	}
	return gen.UpdateChannel200JSONResponse(toChannel(c)), nil
}

func (s *server) DeleteChannel(ctx context.Context, req gen.DeleteChannelRequestObject) (gen.DeleteChannelResponseObject, error) {
	a, err := s.notifyActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.slack.Delete(ctx, a, req.ChannelId); err != nil {
		return nil, err
	}
	return gen.DeleteChannel204Response{}, nil
}

func (s *server) TestChannel(ctx context.Context, req gen.TestChannelRequestObject) (gen.TestChannelResponseObject, error) {
	a, err := s.notifyActor(ctx)
	if err != nil {
		return nil, err
	}
	// Every call counts, whatever its outcome: the limit protects Slack and the audit log.
	if ok, wait := s.slackTests.Reserve(a.TenantID.String()); !ok {
		return nil, &auth.RateLimitedError{RetryAfter: wait}
	}
	if err := s.slack.Test(ctx, a, req.ChannelId); err != nil {
		return nil, err
	}
	return gen.TestChannel204Response{}, nil
}

func (s *server) notifyActor(ctx context.Context) (notify.Actor, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return notify.Actor{}, err
	}
	return notify.Actor{TenantID: p.TenantID, UserID: p.UserID, IP: httpx.ClientIP(httpRequest(ctx), s.cfg.TrustedProxyHops)}, nil
}

func toChannel(c notify.Channel) gen.Channel {
	events := make(gen.ChannelEvents, len(c.Events))
	for i, e := range c.Events {
		events[i] = gen.ChannelEvent(e)
	}
	return gen.Channel{
		Id: c.ID, ProjectId: c.ProjectID, Name: c.Name, Type: gen.ChannelType(c.Type), Events: events, Mention: c.Mention,
		ReminderMinutes: c.ReminderMinutes, Status: gen.ChannelStatus(c.Status), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func fromEvents(in *gen.ChannelEvents) *[]string {
	if in == nil {
		return nil
	}
	out := make([]string, len(*in))
	for i, e := range *in {
		out[i] = string(e)
	}
	return &out
}
