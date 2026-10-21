package api

import (
	"context"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/plans"
)

func (s *server) GetUsage(ctx context.Context, _ gen.GetUsageRequestObject) (gen.GetUsageResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.plans.Usage(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	return gen.GetUsage200JSONResponse{
		Plan:   gen.PlanRef{Key: u.Plan.Key, Name: u.Plan.Name},
		Limits: gen.ResourceCounts{Monitors: u.Limits.Of(plans.ResourceMonitor), Heartbeats: u.Limits.Of(plans.ResourceHeartbeat), Channels: u.Limits.Of(plans.ResourceChannel)},
		Usage:  gen.ResourceCounts{Monitors: u.Counts.Monitors, Heartbeats: u.Counts.Heartbeats, Channels: u.Counts.Channels},
	}, nil
}
