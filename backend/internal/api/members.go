package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// roleError is a request refused by the operation table because the session's role is below the
// operation's minimum role (ADR-0102).
type roleError struct{ required authz.Role }

func (e *roleError) Error() string { return fmt.Sprintf("forbidden: needs role %s", e.required) }

// authorize is the strict-handler middleware that applies the operation table of internal/authz
// to every operation by its operation id: unknown operations are refused, public ones pass, and
// every other one needs a session whose role reaches the operation's minimum. It runs after the
// request validator, so the session (and the CSRF token) were already checked.
func (s *server) authorize(f gen.StrictHandlerFunc, operationID string) gen.StrictHandlerFunc {
	min, known := authz.MinRole(operationID)
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		if !known {
			s.log.ErrorContext(ctx, "operation missing from the authorization table; refused", "operation", operationID)
			return nil, &roleError{required: authz.Owner}
		}
		if min == authz.Public {
			return f(ctx, w, r, req)
		}
		p, err := s.resolve(r)
		if err != nil {
			return nil, err
		}
		if !authz.Can(authz.Actor{UserID: p.UserID, Role: authz.Role(p.Role)}, operationID) {
			s.log.WarnContext(ctx, "operation refused by role", "operation", operationID, "role", p.Role, "required", string(min))
			return nil, &roleError{required: min}
		}
		return f(ctx, w, r, req)
	}
}

func (s *server) memberActor(ctx context.Context) (auth.Actor, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return auth.Actor{}, err
	}
	return auth.Actor{TenantID: p.TenantID, UserID: p.UserID, IP: httpx.ClientIP(httpRequest(ctx), s.cfg.TrustedProxyHops)}, nil
}

func (s *server) ListMembers(ctx context.Context, _ gen.ListMembersRequestObject) (gen.ListMembersResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.auth.ListMembers(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	out := gen.ListMembers200JSONResponse{Items: make([]gen.Member, len(list))}
	for i, m := range list {
		out.Items[i] = toMember(m)
	}
	return out, nil
}

func (s *server) UpdateMember(ctx context.Context, req gen.UpdateMemberRequestObject) (gen.UpdateMemberResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	m, err := s.auth.ChangeRole(ctx, a, req.UserId, string(req.Body.Role))
	if err != nil {
		return nil, err
	}
	return gen.UpdateMember200JSONResponse(toMember(m)), nil
}

func (s *server) RemoveMember(ctx context.Context, req gen.RemoveMemberRequestObject) (gen.RemoveMemberResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.auth.RemoveMember(ctx, a, req.UserId); err != nil {
		return nil, err
	}
	return gen.RemoveMember204Response{}, nil
}

func (s *server) LeaveOrganization(ctx context.Context, _ gen.LeaveOrganizationRequestObject) (gen.LeaveOrganizationResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.auth.Leave(ctx, a); err != nil {
		return nil, err
	}
	cleared := s.sessionCookie("", time.Time{}).String()
	return gen.LeaveOrganization204Response{Headers: gen.LeaveOrganization204ResponseHeaders{SetCookie: &cleared}}, nil
}

func (s *server) DeleteOrganization(ctx context.Context, _ gen.DeleteOrganizationRequestObject) (gen.DeleteOrganizationResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.auth.DeleteOrganization(ctx, a); err != nil {
		return nil, err
	}
	cleared := s.sessionCookie("", time.Time{}).String()
	return gen.DeleteOrganization204Response{Headers: gen.DeleteOrganization204ResponseHeaders{SetCookie: &cleared}}, nil
}

func (s *server) ListInvitations(ctx context.Context, _ gen.ListInvitationsRequestObject) (gen.ListInvitationsResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.auth.ListInvitations(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	out := gen.ListInvitations200JSONResponse{Items: make([]gen.Invitation, len(list))}
	for i, inv := range list {
		out.Items[i] = toInvitation(inv)
	}
	return out, nil
}

// CreateInvitation answers the invitation link once. The token is in the URL fragment, which
// browsers never send to a server, so opening the link does not put it in any access log (R-213).
func (s *server) CreateInvitation(ctx context.Context, req gen.CreateInvitationRequestObject) (gen.CreateInvitationResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	var label string
	if req.Body.Label != nil {
		label = *req.Body.Label
	}
	inv, token, err := s.auth.CreateInvitation(ctx, a, string(req.Body.Role), label)
	if err != nil {
		return nil, err
	}
	return gen.CreateInvitation201JSONResponse{Invitation: toInvitation(inv), InviteUrl: s.cfg.PublicURL + "/invite#" + token}, nil
}

func (s *server) RevokeInvitation(ctx context.Context, req gen.RevokeInvitationRequestObject) (gen.RevokeInvitationResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.auth.RevokeInvitation(ctx, a, req.InvitationId); err != nil {
		return nil, err
	}
	return gen.RevokeInvitation204Response{}, nil
}

func (s *server) LookupInvitation(ctx context.Context, req gen.LookupInvitationRequestObject) (gen.LookupInvitationResponseObject, error) {
	r := httpRequest(ctx)
	pv, err := s.auth.LookupInvitation(ctx, req.Body.Token, s.limiterIP(ctx, r))
	if err != nil {
		return nil, err
	}
	return gen.LookupInvitation200JSONResponse{OrganizationName: pv.OrganizationName, Role: gen.InvitationRole(pv.Role), ExpiresAt: pv.ExpiresAt}, nil
}

func (s *server) AcceptInvitation(ctx context.Context, req gen.AcceptInvitationRequestObject) (gen.AcceptInvitationResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	r := httpRequest(ctx)
	res, err := s.auth.AcceptInvitation(ctx, p, req.Body.Token, s.limiterIP(ctx, r), r.UserAgent())
	if err != nil {
		return nil, err
	}
	logging.SetTenantID(ctx, res.Tenant.ID.String())
	cookie := s.sessionCookie(res.Token, res.ExpiresAt).String()
	return gen.AcceptInvitation200JSONResponse{
		Body:    toSessionInfo(res.SessionInfo),
		Headers: gen.AcceptInvitation200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func (s *server) ListOrganizations(ctx context.Context, _ gen.ListOrganizationsRequestObject) (gen.ListOrganizationsResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.auth.Organizations(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := gen.ListOrganizations200JSONResponse{Items: make([]gen.Organization, len(list))}
	for i, o := range list {
		out.Items[i] = gen.Organization{Id: o.Tenant.ID, Slug: o.Tenant.Slug, Name: o.Tenant.Name, Role: gen.MembershipRole(o.Role),
			Status: gen.TenantStatus(o.Tenant.Status), Current: o.Tenant.ID == p.TenantID}
	}
	return out, nil
}

func (s *server) SwitchOrganization(ctx context.Context, req gen.SwitchOrganizationRequestObject) (gen.SwitchOrganizationResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	r := httpRequest(ctx)
	res, err := s.auth.SwitchOrganization(ctx, p, req.Body.TenantId, httpx.ClientIP(r, s.cfg.TrustedProxyHops), r.UserAgent())
	if err != nil {
		return nil, err
	}
	logging.SetTenantID(ctx, res.Tenant.ID.String())
	cookie := s.sessionCookie(res.Token, res.ExpiresAt).String()
	return gen.SwitchOrganization200JSONResponse{
		Body:    toSessionInfo(res.SessionInfo),
		Headers: gen.SwitchOrganization200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func toMember(m auth.Member) gen.Member {
	return gen.Member{UserId: m.UserID, Login: m.Login, DisplayName: m.DisplayName, Role: gen.MembershipRole(m.Role),
		JoinedAt: m.JoinedAt, IsPlatformAdmin: m.IsPlatformAdmin}
}

func toInvitation(i auth.Invitation) gen.Invitation {
	return gen.Invitation{Id: i.ID, Role: gen.InvitationRole(i.Role), Label: i.Label, Status: gen.InvitationStatus(i.Status),
		CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt, CreatedBy: i.CreatedBy, AcceptedBy: i.AcceptedBy,
		AcceptedAt: i.AcceptedAt, RevokedAt: i.RevokedAt}
}
