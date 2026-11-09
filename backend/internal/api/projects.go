package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/projects"
)

// accessSlot caches the actor's project access for one request: the role table check
// (authorize) and the services ask for it, and it costs one query for members and viewers.
type accessSlot struct {
	once  sync.Once
	actor authz.Actor
	err   error
}

type accessKey struct{}

func withAccessSlot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accessKey{}, &accessSlot{})))
	})
}

// access resolves the actor of the request with its role in each project it belongs to
// (ADR-0102: once per request, read from the database, so a change applies on the next request).
func (s *server) access(ctx context.Context) (authz.Actor, error) {
	r := httpRequest(ctx)
	slot, _ := r.Context().Value(accessKey{}).(*accessSlot)
	if slot == nil {
		return authz.Actor{}, fmt.Errorf("api: access slot missing")
	}
	slot.once.Do(func() {
		p, err := s.resolve(r)
		if err != nil {
			slot.err = err
			return
		}
		a := authz.Actor{UserID: p.UserID, Role: authz.Role(p.Role)}
		a.Projects, slot.err = s.projects.Access(ctx, p.TenantID, p.UserID, a.Role)
		slot.actor = a
	})
	return slot.actor, slot.err
}

// scope is the set of projects whose data the request may read.
func (s *server) scope(ctx context.Context) (authz.Scope, error) {
	a, err := s.access(ctx)
	if err != nil {
		return authz.Scope{}, err
	}
	return a.Scope(), nil
}

// projectFilter narrows a list to one project; projects.ErrNotFound (404) when the actor does
// not see it or it does not exist.
func (s *server) projectFilter(ctx context.Context, id *uuid.UUID) (authz.Scope, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return authz.Scope{}, err
	}
	if id == nil {
		return a.Access.Scope(), nil
	}
	if _, err := s.projects.Get(ctx, a, *id); err != nil {
		return authz.Scope{}, err
	}
	return a.Access.Scope().Narrow(*id), nil
}

func (s *server) projectActor(ctx context.Context) (projects.Actor, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return projects.Actor{}, err
	}
	a, err := s.access(ctx)
	if err != nil {
		return projects.Actor{}, err
	}
	return projects.Actor{TenantID: p.TenantID, IP: httpx.ClientIP(httpRequest(ctx), s.cfg.TrustedProxyHops), Access: a}, nil
}

func (s *server) ListProjects(ctx context.Context, _ gen.ListProjectsRequestObject) (gen.ListProjectsResponseObject, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.projects.List(ctx, a)
	if err != nil {
		return nil, err
	}
	out := gen.ListProjects200JSONResponse{Items: make([]gen.Project, len(list))}
	for i, p := range list {
		out.Items[i] = toProject(p)
	}
	return out, nil
}

func (s *server) CreateProject(ctx context.Context, req gen.CreateProjectRequestObject) (gen.CreateProjectResponseObject, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.projects.Create(ctx, a, projects.CreateInput{Name: req.Body.Name, Slug: req.Body.Slug})
	if err != nil {
		return nil, err
	}
	return gen.CreateProject201JSONResponse(toProject(p)), nil
}

func (s *server) GetProject(ctx context.Context, req gen.GetProjectRequestObject) (gen.GetProjectResponseObject, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.projects.Get(ctx, a, req.ProjectId)
	if err != nil {
		return nil, err
	}
	return gen.GetProject200JSONResponse(toProject(p)), nil
}

func (s *server) UpdateProject(ctx context.Context, req gen.UpdateProjectRequestObject) (gen.UpdateProjectResponseObject, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.projects.Update(ctx, a, req.ProjectId, projects.UpdateInput{Name: req.Body.Name, Slug: req.Body.Slug})
	if err != nil {
		return nil, err
	}
	return gen.UpdateProject200JSONResponse(toProject(p)), nil
}

func (s *server) DeleteProject(ctx context.Context, req gen.DeleteProjectRequestObject) (gen.DeleteProjectResponseObject, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.projects.Delete(ctx, a, req.ProjectId); err != nil {
		return nil, err
	}
	return gen.DeleteProject204Response{}, nil
}

func (s *server) ListProjectMembers(ctx context.Context, req gen.ListProjectMembersRequestObject) (gen.ListProjectMembersResponseObject, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.projects.Members(ctx, a, req.ProjectId)
	if err != nil {
		return nil, err
	}
	showLogins := a.Access.Role.AtLeast(authz.Admin) // L-278
	out := gen.ListProjectMembers200JSONResponse{Items: make([]gen.ProjectMember, len(list))}
	for i, m := range list {
		out.Items[i] = toProjectMember(m, showLogins)
	}
	return out, nil
}

func (s *server) PutProjectMember(ctx context.Context, req gen.PutProjectMemberRequestObject) (gen.PutProjectMemberResponseObject, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return nil, err
	}
	var role *authz.Role
	if req.Body.Role != nil {
		r := authz.Role(*req.Body.Role)
		role = &r
	}
	m, err := s.projects.PutMember(ctx, a, req.ProjectId, req.UserId, role)
	if err != nil {
		return nil, err
	}
	return gen.PutProjectMember200JSONResponse(toProjectMember(m, true)), nil
}

func (s *server) RemoveProjectMember(ctx context.Context, req gen.RemoveProjectMemberRequestObject) (gen.RemoveProjectMemberResponseObject, error) {
	a, err := s.projectActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.projects.RemoveMember(ctx, a, req.ProjectId, req.UserId); err != nil {
		return nil, err
	}
	return gen.RemoveProjectMember204Response{}, nil
}

func toProject(p projects.Project) gen.Project {
	return gen.Project{Id: p.ID, Name: p.Name, Slug: p.Slug, IsDefault: p.IsDefault, AllMembers: p.AllMembers,
		Role: gen.MembershipRole(p.Role), MonitorCount: p.MonitorCount, ChannelCount: p.ChannelCount,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

// toProjectMember maps a project member; showLogin is false for callers below admin (L-278).
func toProjectMember(m projects.Member, showLogin bool) gen.ProjectMember {
	out := gen.ProjectMember{UserId: m.UserID, Login: loginFor(m.Login, showLogin), DisplayName: m.DisplayName,
		OrganizationRole: gen.MembershipRole(m.OrganizationRole), EffectiveRole: gen.MembershipRole(m.EffectiveRole),
		Explicit: m.Explicit, AllProjects: m.AllProjects}
	if m.ProjectRole != nil {
		r := gen.ProjectRole(*m.ProjectRole)
		out.ProjectRole = &r
	}
	return out
}
