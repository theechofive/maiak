package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/oauth"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// OAuthCookie carries the signed state of an OAuth flow from start to callback (ADR-0104).
const OAuthCookie = "maiak_oauth"

const oauthCookiePath = "/api/v1/auth/oauth/"

// Callback outcomes: the error codes of the /login and /profile redirects. They are the only
// thing the browser learns; provider text is never reflected.
const (
	oauthCancelled         = "cancelled"
	oauthFailed            = "failed"
	oauthStateInvalid      = "state_invalid"
	oauthEmailUnverified   = "email_unverified"
	oauthEmailUnusable     = "email_unusable"
	oauthNotInvited        = "not_invited"
	oauthAccountExists     = "account_exists"
	oauthInvitationInvalid = "invitation_invalid"
	oauthAlreadyMember     = "already_member"
	oauthPlanLimit         = "plan_limit"
	oauthSuspended         = "suspended"
	oauthRateLimited       = "rate_limited"
	oauthIdentityInUse     = "identity_in_use"
	oauthAlreadyLinked     = "already_linked"
	oauthSessionExpired    = "session_expired"
)

func (s *server) GetAuthProviders(_ context.Context, _ gen.GetAuthProvidersRequestObject) (gen.GetAuthProvidersResponseObject, error) {
	out := gen.GetAuthProviders200JSONResponse{
		Providers:            []gen.AuthProvider{},
		PasswordLoginEnabled: s.cfg.PasswordLoginEnabled,
		RegistrationEnabled:  s.cfg.RegistrationEnabled,
	}
	if s.oauth != nil {
		for _, p := range s.oauth.Providers() {
			out.Providers = append(out.Providers, gen.AuthProvider{Id: gen.OAuthProvider(p.Name()), Name: p.DisplayName()})
		}
	}
	return out, nil
}

func (s *server) StartOAuth(ctx context.Context, req gen.StartOAuthRequestObject) (gen.StartOAuthResponseObject, error) {
	provider := string(req.Provider)
	if s.oauth == nil {
		return nil, errNotFound
	}
	if _, ok := s.oauth.Provider(provider); !ok {
		return nil, errNotFound
	}
	r := httpRequest(ctx)
	intent := oauth.IntentLogin
	if req.Body.Intent != nil && *req.Body.Intent == gen.Link {
		intent = oauth.IntentLink
	}
	var userID uuid.UUID
	var inviteHash []byte
	switch intent {
	case oauth.IntentLink:
		if req.Body.InviteToken != nil {
			return nil, &badRequestError{detail: "inviteToken is only allowed with intent login"}
		}
		// The operation is public in the spec (a sign-in has no session); a link needs one, with
		// the CSRF token like every other unsafe request of a session.
		p, err := s.resolve(r)
		if err != nil {
			return nil, err
		}
		if !auth.CSRFValid(p.CSRFToken, r.Header.Get(csrfHeader)) {
			return nil, auth.ErrCSRF
		}
		userID = p.UserID
	default:
		if req.Body.InviteToken != nil {
			h, err := s.auth.CheckInvitation(ctx, *req.Body.InviteToken, s.limiterIP(ctx, r))
			if err != nil {
				return nil, err
			}
			inviteHash = h
		}
	}
	authURL, state, err := s.oauth.Start(ctx, provider, intent, userID, inviteHash)
	if err != nil {
		// Discovery failed (Google unreachable): a problem for the operator, not the user.
		s.log.ErrorContext(ctx, "oauth start failed", "provider", provider, "error", err)
		return nil, errOAuthUnavailable
	}
	cookie := s.oauthStateCookie(state).String()
	return gen.StartOAuth200JSONResponse{
		Body:    gen.OAuthStart{AuthorizationUrl: authURL},
		Headers: gen.StartOAuth200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

// oauthRedirect is the callback's answer: a 303 with several Set-Cookie headers (the generated
// response type carries only one).
type oauthRedirect struct {
	location string
	cookies  []*http.Cookie
}

func (o oauthRedirect) VisitOauthCallbackResponse(w http.ResponseWriter) error {
	for _, c := range o.cookies {
		w.Header().Add("Set-Cookie", c.String())
	}
	w.Header().Set("Location", o.location)
	w.WriteHeader(http.StatusSeeOther)
	return nil
}

func (s *server) OauthCallback(ctx context.Context, req gen.OauthCallbackRequestObject) (gen.OauthCallbackResponseObject, error) {
	r := httpRequest(ctx)
	provider := string(req.Provider)
	cleared := s.oauthStateCookie("")
	ip := s.limiterIP(ctx, r)
	fail := func(target, code string) (gen.OauthCallbackResponseObject, error) {
		return oauthRedirect{location: target + "?error=" + code, cookies: []*http.Cookie{cleared}}, nil
	}
	// Every callback counts against the login's per-IP limit until it succeeds, a forged or
	// replayed one included (each failure writes an audit row).
	release, err := s.auth.ReserveOAuthAttempt(ctx, ip)
	if err != nil {
		return fail("/login", oauthRateLimited)
	}

	var cookie string
	if c, err := r.Cookie(OAuthCookie); err == nil {
		cookie = c.Value
	}
	var st oauth.State
	err = errNotFound
	if s.oauth != nil {
		st, err = s.oauth.ParseState(provider, cookie, deref(req.Params.State))
	}
	if err != nil {
		// Without a valid state nothing tells us whether this was a link: answer like a sign-in.
		s.auth.RecordOAuthFailure(ctx, provider, oauthStateInvalid, uuid.Nil, nil, ip)
		return fail("/login", oauthStateInvalid)
	}
	target := "/login"
	if st.Intent == oauth.IntentLink {
		target = "/profile"
	}
	if req.Params.Error != nil {
		// The user cancelled at the provider (or it refused); its error text is not shown.
		s.auth.RecordOAuthFailure(ctx, provider, oauthCancelled, uuid.Nil, nil, ip)
		return fail(target, oauthCancelled)
	}
	id, err := s.oauth.Exchange(ctx, st, deref(req.Params.Code))
	if err != nil {
		code := oauthFailed
		if errors.Is(err, oauth.ErrEmailUnverified) {
			code = oauthEmailUnverified
		} else {
			s.log.WarnContext(ctx, "oauth code exchange failed", "provider", provider, "error", err)
		}
		s.auth.RecordOAuthFailure(ctx, provider, code, uuid.Nil, nil, ip)
		return fail(target, code)
	}
	identity := auth.OAuthIdentity{Provider: id.Provider, Subject: id.Subject, Email: id.Email, HostedDomain: id.HostedDomain, DisplayName: id.DisplayName}

	if st.Intent == oauth.IntentLink {
		p, err := s.resolve(r)
		if err != nil || p.UserID != st.UserID {
			// Signed out, or another user signed in, since the link started.
			s.auth.RecordOAuthFailure(ctx, provider, oauthSessionExpired, uuid.Nil, nil, ip)
			return fail(target, oauthSessionExpired)
		}
		if err := s.auth.LinkIdentity(ctx, p, identity, ip); err != nil {
			if code, ok := oauthErrorCode(err); ok {
				return fail(target, code)
			}
			s.log.ErrorContext(ctx, "oauth link failed", "provider", provider, "error", err)
			return fail(target, oauthFailed)
		}
		release()
		return oauthRedirect{location: "/profile?linked=" + url.QueryEscape(provider), cookies: []*http.Cookie{cleared}}, nil
	}

	res, err := s.auth.OAuthLogin(ctx, auth.OAuthLoginInput{
		Identity: identity, InviteHash: st.InviteHash, RegistrationEnabled: s.cfg.RegistrationEnabled,
		IP: ip, UserAgent: r.UserAgent(),
	})
	if err != nil {
		if code, ok := oauthErrorCode(err); ok {
			return fail(target, code)
		}
		s.log.ErrorContext(ctx, "oauth login failed", "provider", provider, "error", err)
		return fail(target, oauthFailed)
	}
	release()
	logging.SetTenantID(ctx, res.Tenant.ID.String())
	return oauthRedirect{location: "/", cookies: []*http.Cookie{cleared, s.sessionCookie(res.Token, res.ExpiresAt)}}, nil
}

// oauthErrorCode maps the domain errors of a sign-in or link to a redirect code.
func oauthErrorCode(err error) (string, bool) {
	var limit *plans.LimitError
	switch {
	case errors.Is(err, auth.ErrOAuthNotInvited):
		return oauthNotInvited, true
	case errors.Is(err, auth.ErrOAuthAccountExists):
		return oauthAccountExists, true
	case errors.Is(err, auth.ErrOAuthEmailUnusable):
		return oauthEmailUnusable, true
	case errors.Is(err, auth.ErrInvitationInvalid):
		return oauthInvitationInvalid, true
	case errors.Is(err, auth.ErrAlreadyMember):
		return oauthAlreadyMember, true
	case errors.As(err, &limit):
		return oauthPlanLimit, true
	case errors.Is(err, auth.ErrAccountSuspended):
		return oauthSuspended, true
	case errors.Is(err, auth.ErrIdentityInUse):
		return oauthIdentityInUse, true
	case errors.Is(err, auth.ErrAlreadyLinked):
		return oauthAlreadyLinked, true
	case errors.Is(err, auth.ErrUnauthenticated):
		return oauthSessionExpired, true
	case errors.Is(err, auth.ErrInvalidCredentials):
		return oauthFailed, true
	case errors.Is(err, auth.ErrRateLimited):
		return oauthRateLimited, true
	}
	return "", false
}

// oauthStateCookie builds the state cookie, scoped to the OAuth paths; an empty value expires it.
// SameSite=Lax: the callback is a top-level GET navigation from the provider.
func (s *server) oauthStateCookie(value string) *http.Cookie {
	c := &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite=Lax are literal, Secure is configurable (default true)
		Name:     OAuthCookie,
		Value:    value,
		Path:     oauthCookiePath,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	if value == "" {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(oauth.StateTTL / time.Second)
	}
	return c
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *server) ListIdentities(ctx context.Context, _ gen.ListIdentitiesRequestObject) (gen.ListIdentitiesResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	m, err := s.auth.ListIdentities(ctx, p)
	if err != nil {
		return nil, err
	}
	out := gen.ListIdentities200JSONResponse{Items: make([]gen.Identity, len(m.Identities)),
		PasswordSet: m.PasswordSet, PasswordLoginEnabled: m.PasswordLoginEnabled}
	for i, id := range m.Identities {
		out.Items[i] = gen.Identity{Provider: gen.OAuthProvider(id.Provider), Email: id.Email, CreatedAt: id.CreatedAt, LastLoginAt: id.LastLoginAt}
	}
	return out, nil
}

func (s *server) UnlinkIdentity(ctx context.Context, req gen.UnlinkIdentityRequestObject) (gen.UnlinkIdentityResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.auth.UnlinkIdentity(ctx, p, string(req.Provider), httpx.ClientIP(httpRequest(ctx), s.cfg.TrustedProxyHops)); err != nil {
		return nil, err
	}
	return gen.UnlinkIdentity204Response{}, nil
}

func (s *server) ListAuthDomains(ctx context.Context, _ gen.ListAuthDomainsRequestObject) (gen.ListAuthDomainsResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.auth.ListAuthDomains(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	out := gen.ListAuthDomains200JSONResponse{Items: make([]gen.AuthDomain, len(list))}
	for i, d := range list {
		out.Items[i] = toAuthDomain(d)
	}
	return out, nil
}

func (s *server) CreateAuthDomain(ctx context.Context, req gen.CreateAuthDomainRequestObject) (gen.CreateAuthDomainResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	role := ""
	if req.Body.DefaultRole != nil {
		role = string(*req.Body.DefaultRole)
	}
	enabled := req.Body.Enabled == nil || *req.Body.Enabled
	d, err := s.auth.CreateAuthDomain(ctx, a, req.Body.Domain, role, enabled)
	if err != nil {
		return nil, err
	}
	return gen.CreateAuthDomain201JSONResponse(toAuthDomain(d)), nil
}

func (s *server) UpdateAuthDomain(ctx context.Context, req gen.UpdateAuthDomainRequestObject) (gen.UpdateAuthDomainResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	var role *string
	if req.Body.DefaultRole != nil {
		r := string(*req.Body.DefaultRole)
		role = &r
	}
	d, err := s.auth.UpdateAuthDomain(ctx, a, req.DomainId, role, req.Body.Enabled)
	if err != nil {
		return nil, err
	}
	return gen.UpdateAuthDomain200JSONResponse(toAuthDomain(d)), nil
}

func (s *server) DeleteAuthDomain(ctx context.Context, req gen.DeleteAuthDomainRequestObject) (gen.DeleteAuthDomainResponseObject, error) {
	a, err := s.memberActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.auth.DeleteAuthDomain(ctx, a, req.DomainId); err != nil {
		return nil, err
	}
	return gen.DeleteAuthDomain204Response{}, nil
}

func toAuthDomain(d auth.AuthDomain) gen.AuthDomain {
	return gen.AuthDomain{Id: openapi_types.UUID(d.ID), Domain: d.Domain, DefaultRole: gen.AuthDomainRole(d.DefaultRole),
		Enabled: d.Enabled, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
}
