package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/statuspages"
)

// pageActor is the actor of a status page operation.
func (s *server) pageActor(ctx context.Context) (statuspages.Actor, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return statuspages.Actor{}, err
	}
	a, err := s.access(ctx)
	if err != nil {
		return statuspages.Actor{}, err
	}
	return statuspages.Actor{TenantID: p.TenantID, IP: httpx.ClientIP(httpRequest(ctx), s.cfg.TrustedProxyHops), Access: a}, nil
}

func (s *server) ListStatusPages(ctx context.Context, _ gen.ListStatusPagesRequestObject) (gen.ListStatusPagesResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.pages.List(ctx, a)
	if err != nil {
		return nil, err
	}
	out := gen.ListStatusPages200JSONResponse{Items: make([]gen.StatusPage, len(list))}
	for i, p := range list {
		out.Items[i] = s.toStatusPage(p)
	}
	return out, nil
}

func (s *server) CreateStatusPage(ctx context.Context, req gen.CreateStatusPageRequestObject) (gen.CreateStatusPageResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	in := statuspages.CreateInput{Slug: req.Body.Slug, Title: req.Body.Title}
	if req.Body.Description != nil {
		in.Description = *req.Body.Description
	}
	if req.Body.LogoUrl != nil {
		in.LogoURL = *req.Body.LogoUrl
	}
	if req.Body.AccentColor != nil {
		in.AccentColor = *req.Body.AccentColor
	}
	p, err := s.pages.Create(ctx, a, in)
	if err != nil {
		return nil, err
	}
	return gen.CreateStatusPage201JSONResponse(s.toStatusPage(p)), nil
}

func (s *server) GetStatusPage(ctx context.Context, req gen.GetStatusPageRequestObject) (gen.GetStatusPageResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.pages.Get(ctx, a, req.StatusPageId)
	if err != nil {
		return nil, err
	}
	return gen.GetStatusPage200JSONResponse(s.toStatusPage(p)), nil
}

func (s *server) UpdateStatusPage(ctx context.Context, req gen.UpdateStatusPageRequestObject) (gen.UpdateStatusPageResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	p, err := s.pages.Update(ctx, a, req.StatusPageId, statuspages.UpdateInput{
		Slug: b.Slug, Title: b.Title, Description: b.Description, LogoURL: b.LogoUrl, AccentColor: b.AccentColor, Published: b.Published,
	})
	if err != nil {
		return nil, err
	}
	return gen.UpdateStatusPage200JSONResponse(s.toStatusPage(p)), nil
}

func (s *server) DeleteStatusPage(ctx context.Context, req gen.DeleteStatusPageRequestObject) (gen.DeleteStatusPageResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.pages.Delete(ctx, a, req.StatusPageId); err != nil {
		return nil, err
	}
	return gen.DeleteStatusPage204Response{}, nil
}

func (s *server) SetStatusPagePassword(ctx context.Context, req gen.SetStatusPagePasswordRequestObject) (gen.SetStatusPagePasswordResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.pages.SetPassword(ctx, a, req.StatusPageId, &req.Body.Password); err != nil {
		return nil, err
	}
	return gen.SetStatusPagePassword204Response{}, nil
}

func (s *server) ClearStatusPagePassword(ctx context.Context, req gen.ClearStatusPagePasswordRequestObject) (gen.ClearStatusPagePasswordResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.pages.SetPassword(ctx, a, req.StatusPageId, nil); err != nil {
		return nil, err
	}
	return gen.ClearStatusPagePassword204Response{}, nil
}

func (s *server) ListStatusPageComponents(ctx context.Context, req gen.ListStatusPageComponentsRequestObject) (gen.ListStatusPageComponentsResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.pages.Components(ctx, a, req.StatusPageId)
	if err != nil {
		return nil, err
	}
	return gen.ListStatusPageComponents200JSONResponse(toComponentList(list)), nil
}

func (s *server) PutStatusPageComponents(ctx context.Context, req gen.PutStatusPageComponentsRequestObject) (gen.PutStatusPageComponentsResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	in := make([]statuspages.ComponentInput, len(req.Body.Items))
	for i, c := range req.Body.Items {
		in[i] = statuspages.ComponentInput{MonitorID: c.MonitorId, PublicName: c.PublicName, GroupName: c.GroupName}
	}
	list, err := s.pages.PutComponents(ctx, a, req.StatusPageId, in)
	if err != nil {
		return nil, err
	}
	return gen.PutStatusPageComponents200JSONResponse(toComponentList(list)), nil
}

func (s *server) PreviewStatusPage(ctx context.Context, req gen.PreviewStatusPageRequestObject) (gen.PreviewStatusPageResponseObject, error) {
	a, err := s.pageActor(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := s.pages.Preview(ctx, a, req.StatusPageId)
	if err != nil {
		return nil, err
	}
	return previewResponse{snap: snap}, nil
}

// previewResponse writes the snapshot with the same encoder as the public JSON, so the preview is
// byte for byte what /status/<slug>.json would serve.
type previewResponse struct{ snap statuspages.Snapshot }

func (r previewResponse) VisitPreviewStatusPageResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(r.snap)
}

func (s *server) toStatusPage(p statuspages.Page) gen.StatusPage {
	return gen.StatusPage{Id: p.ID, Slug: p.Slug, Title: p.Title, Description: p.Description, LogoUrl: p.LogoURL, AccentColor: p.AccentColor,
		Published: p.Published, PasswordProtected: p.PasswordProtected, PublicUrl: s.cfg.PublicURL + "/status/" + p.Slug,
		ComponentCount: p.ComponentCount, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

func toComponentList(list []statuspages.ComponentConfig) gen.StatusPageComponentList {
	out := gen.StatusPageComponentList{Items: make([]gen.StatusPageComponent, len(list))}
	for i, c := range list {
		out.Items[i] = gen.StatusPageComponent{MonitorId: c.MonitorID, MonitorName: c.MonitorName, MonitorType: gen.MonitorType(c.MonitorType),
			ProjectId: &c.ProjectID, PublicName: c.PublicName, GroupName: c.GroupName, Position: c.Position}
	}
	return out
}
