// Package api is the HTTP layer of cmd/api. It implements the strict server interface generated
// from api/openapi.yaml, validates every request against the spec, and maps domain errors to
// RFC 9457 problems in one place (errors.go).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	middleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/admin"
	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/oauth"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/projects"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/statuspages"
)

// SessionCookie is the name of the session cookie.
const SessionCookie = "maiak_session"

const (
	csrfHeader   = "X-CSRF-Token"
	maxBodyBytes = 64 << 10
	// xffWarnInterval rate-limits the missing X-Forwarded-For warning.
	xffWarnInterval = time.Minute
	// slackTestInterval is the minimum spacing of POST /settings/slack/test per tenant: each call
	// makes an outbound request to Slack and writes an audit row.
	slackTestInterval = 5 * time.Second
)

// Config holds the HTTP-layer settings.
type Config struct {
	Version          string
	Commit           string
	CookieSecure     bool
	TrustedProxyHops int
	// RegistrationEnabled is the kill switch of POST /auth/register (MAIAK_REGISTRATION_ENABLED).
	RegistrationEnabled bool
	// PublicURL is MAIAK_PUBLIC_URL (no trailing slash), the base of heartbeat ping URLs.
	PublicURL string
	// PasswordLoginEnabled is MAIAK_PASSWORD_LOGIN_ENABLED (ADR-0104); auth enforces it, the api
	// only reports it.
	PasswordLoginEnabled bool
}

// Deps are the collaborators of the HTTP layer.
type Deps struct {
	Auth     *auth.Service
	Monitors *monitors.Service
	Queries  *queries.Service
	Slack    *notify.Service
	Plans    *plans.Service
	Projects *projects.Service
	// StatusPages configures the public status page (ADR-0103).
	StatusPages *statuspages.Service
	// Admin serves /api/v1/admin (P-04); nil answers those operations with 500.
	Admin *admin.Service
	// OAuth runs the OAuth sign-in flows (ADR-0104); nil means no provider is configured.
	OAuth *oauth.Flow
	// Entitlements reports the paid features of an organization (ADR-0105); nil means none.
	Entitlements EntitlementsProvider
	Logger       *slog.Logger
	Metrics      *metrics.HTTP
	// Pings counts heartbeat pings; nil keeps the counts unregistered.
	Pings *PingMetrics
	// Clock drives the in-process rate limits; nil means the system clock.
	Clock clock.Clock
}

// EntitlementsProvider answers which paid features an organization may use (ADR-0105). Declared
// here, by its consumer; internal/entitlements implements it.
type EntitlementsProvider interface {
	Features(ctx context.Context, tenantID uuid.UUID) ([]string, error)
}

type server struct {
	cfg      Config
	auth     *auth.Service
	monitors *monitors.Service
	queries  *queries.Service
	slack    *notify.Service
	plans    *plans.Service
	projects *projects.Service
	pages    *statuspages.Service
	admin    *admin.Service
	oauth    *oauth.Flow
	// entitlements is nil when no provider is wired: every organization has no features.
	entitlements EntitlementsProvider
	log          *slog.Logger
	// slackTests limits Slack test messages per tenant (per api replica, like the login limit).
	slackTests *auth.Limiter
	pingLimits *pingLimits
	pings      *PingMetrics
	// lastXFFWarn is the UnixNano time of the last missing X-Forwarded-For warning.
	lastXFFWarn atomic.Int64
}

var _ gen.StrictServerInterface = (*server)(nil)

// NewHandler builds the public HTTP handler.
func NewHandler(cfg Config, deps Deps) (http.Handler, error) {
	spec, err := gen.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("api: load embedded spec: %w", err)
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.System{}
	}
	pings := deps.Pings
	if pings == nil {
		pings = NewPingMetrics(prometheus.NewRegistry())
	}
	s := &server{cfg: cfg, auth: deps.Auth, monitors: deps.Monitors, queries: deps.Queries, slack: deps.Slack, plans: deps.Plans, projects: deps.Projects, pages: deps.StatusPages, admin: deps.Admin, oauth: deps.OAuth, entitlements: deps.Entitlements, log: deps.Logger,
		slackTests: auth.NewLimiter(1, slackTestInterval, 10_000, clk),
		pingLimits: newPingLimits(clk),
		pings:      pings}

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.AccessLog(deps.Logger), httpx.Recover(deps.Logger))
	if deps.Metrics != nil {
		r.Use(deps.Metrics.Middleware)
	}
	r.Use(apiHeaders, httpx.MaxBody(maxBodyBytes), withSessionSlot, withAccessSlot)
	r.Use(middleware.OapiRequestValidatorWithOptions(spec, &middleware.Options{
		// Host matching against `servers` is pointless behind the gateway.
		DoNotValidateServers: true,
		Options: openapi3filter.Options{
			AuthenticationFunc: s.authenticate,
		},
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, r *http.Request, _ middleware.ErrorHandlerOpts) {
			writeError(w, r, s.log, err)
		},
	}))
	r.Use(s.requirePlatformAdmin)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { writeError(w, r, s.log, errNotFound) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { writeError(w, r, s.log, errMethodNotAllowed) })

	badRequest := func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, r, s.log, &badRequestError{detail: err.Error()})
	}
	strict := gen.NewStrictHandlerWithOptions(s, []gen.StrictMiddlewareFunc{s.authorize, withHTTPRequest}, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  badRequest,
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) { writeError(w, r, s.log, err) },
	})
	return gen.HandlerWithOptions(strict, gen.ChiServerOptions{
		BaseRouter:       r,
		ErrorHandlerFunc: badRequest,
	}), nil
}

// apiHeaders keeps API responses (sessions, CSRF tokens, tenant data) out of shared caches and
// stops browsers from sniffing a JSON or problem+json body into something executable.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

type httpRequestKey struct{}

// withHTTPRequest gives strict handlers access to the request (client IP, user agent, cookies).
func withHTTPRequest(f gen.StrictHandlerFunc, _ string) gen.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		return f(context.WithValue(ctx, httpRequestKey{}, r), w, r, req)
	}
}

func httpRequest(ctx context.Context) *http.Request {
	r, _ := ctx.Value(httpRequestKey{}).(*http.Request)
	if r == nil {
		// Only reachable if a handler is called outside the strict wrapper.
		panic("api: http request missing from context")
	}
	return r
}

// sessionSlot carries the authenticated principal from the validator's AuthenticationFunc
// (which cannot change the request context) to the handlers. It resolves at most once per
// request even when several security schemes are checked.
type sessionSlot struct {
	once      sync.Once
	principal auth.Principal
	err       error
}

type slotKey struct{}

func withSessionSlot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), slotKey{}, &sessionSlot{})))
	})
}

func (s *server) resolve(r *http.Request) (auth.Principal, error) {
	slot, _ := r.Context().Value(slotKey{}).(*sessionSlot)
	if slot == nil {
		return auth.Principal{}, errors.New("api: session slot missing")
	}
	slot.once.Do(func() {
		c, err := r.Cookie(SessionCookie)
		if err != nil {
			slot.err = auth.ErrUnauthenticated
			return
		}
		slot.principal, slot.err = s.auth.Authenticate(r.Context(), c.Value)
		if slot.err == nil {
			// Every later log line of the request, including the access log, carries the tenant.
			logging.SetTenantID(r.Context(), slot.principal.TenantID.String())
		}
	})
	return slot.principal, slot.err
}

// principal returns the authenticated session of the request handled under ctx.
func (s *server) principal(ctx context.Context) (auth.Principal, error) {
	return s.resolve(httpRequest(ctx))
}

// authenticate is called by the request validator for every security scheme the matched
// operation requires, so which endpoints need a session is decided by the spec.
func (s *server) authenticate(_ context.Context, in *openapi3filter.AuthenticationInput) error {
	r := in.RequestValidationInput.Request
	p, err := s.resolve(r)
	if err != nil {
		return err
	}
	switch in.SecuritySchemeName {
	case "cookieAuth":
		// Defence in depth: unsafe methods need the CSRF token even if the spec forgets it.
		if isUnsafe(r.Method) && !auth.CSRFValid(p.CSRFToken, r.Header.Get(csrfHeader)) {
			return auth.ErrCSRF
		}
	case "csrfToken":
		if !auth.CSRFValid(p.CSRFToken, r.Header.Get(csrfHeader)) {
			return auth.ErrCSRF
		}
	default:
		return fmt.Errorf("api: unsupported security scheme %q", in.SecuritySchemeName)
	}
	return nil
}

func isUnsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	}
	return true
}

func (s *server) GetHealth(_ context.Context, _ gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	return gen.GetHealth200JSONResponse{Status: gen.Ok, Version: s.cfg.Version, Commit: s.cfg.Commit}, nil
}

func (s *server) GetPublicConfig(_ context.Context, _ gen.GetPublicConfigRequestObject) (gen.GetPublicConfigResponseObject, error) {
	return gen.GetPublicConfig200JSONResponse{RegistrationEnabled: s.cfg.RegistrationEnabled}, nil
}

// limiterIP is the client address the login and registration limits are keyed by.
func (s *server) limiterIP(ctx context.Context, r *http.Request) netip.Addr {
	ip, trusted := httpx.ForwardedClientIP(r, s.cfg.TrustedProxyHops)
	if !trusted {
		// The peer is the proxy: limiting by it would let one client lock everybody out.
		// An invalid address makes auth use one shared bucket for such requests.
		s.warnMissingXFF(ctx)
		return netip.Addr{}
	}
	return ip
}

func (s *server) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	r := httpRequest(ctx)
	res, err := s.auth.Login(ctx, auth.LoginInput{
		Login:     req.Body.Login,
		Password:  req.Body.Password,
		IP:        s.limiterIP(ctx, r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		return nil, err
	}
	logging.SetTenantID(ctx, res.Tenant.ID.String())
	cookie := s.sessionCookie(res.Token, res.ExpiresAt).String()
	return gen.Login200JSONResponse{
		Body:    s.toSessionInfo(ctx, res.SessionInfo),
		Headers: gen.Login200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func (s *server) Register(ctx context.Context, req gen.RegisterRequestObject) (gen.RegisterResponseObject, error) {
	var invite string
	if req.Body.InviteToken != nil {
		invite = *req.Body.InviteToken
	}
	// An invitation is its own authorization (ADR-0102): it works with registration turned off.
	if !s.cfg.RegistrationEnabled && invite == "" {
		return nil, errRegistrationDisabled
	}
	r := httpRequest(ctx)
	res, err := s.auth.Register(ctx, auth.RegisterInput{
		Email:       req.Body.Email,
		Password:    req.Body.Password,
		IP:          s.limiterIP(ctx, r),
		UserAgent:   r.UserAgent(),
		InviteToken: invite,
	})
	if err != nil {
		return nil, err
	}
	logging.SetTenantID(ctx, res.Tenant.ID.String())
	cookie := s.sessionCookie(res.Token, res.ExpiresAt).String()
	return gen.Register201JSONResponse{
		Body:    s.toSessionInfo(ctx, res.SessionInfo),
		Headers: gen.Register201ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func (s *server) warnMissingXFF(ctx context.Context) {
	now := time.Now().UnixNano()
	last := s.lastXFFWarn.Load()
	if now-last < int64(xffWarnInterval) || !s.lastXFFWarn.CompareAndSwap(last, now) {
		return
	}
	s.log.WarnContext(ctx, "login or registration without a valid X-Forwarded-For although MAIAK_TRUSTED_PROXY_HOPS > 0; rate limiting it under a shared key",
		"trusted_proxy_hops", s.cfg.TrustedProxyHops)
}

func (s *server) Logout(ctx context.Context, _ gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.auth.Logout(ctx, p, httpx.ClientIP(httpRequest(ctx), s.cfg.TrustedProxyHops)); err != nil {
		return nil, err
	}
	cleared := s.sessionCookie("", time.Time{}).String()
	return gen.Logout204Response{Headers: gen.Logout204ResponseHeaders{SetCookie: &cleared}}, nil
}

func (s *server) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	info, err := s.auth.Me(ctx, p)
	if err != nil {
		return nil, err
	}
	return gen.GetMe200JSONResponse(s.toSessionInfo(ctx, info)), nil
}

// sessionCookie builds the session cookie; an empty token expires it.
func (s *server) sessionCookie(token string, expires time.Time) *http.Cookie {
	// Secure comes from MAIAK_COOKIE_SECURE (default true); gosec G124 only accepts a literal true.
	c := &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite=Lax are literal, Secure is configurable (default true)
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	if token == "" {
		c.MaxAge = -1
	} else {
		c.Expires = expires
		c.MaxAge = int(time.Until(expires).Seconds())
	}
	return c
}

// toSessionInfo answers a session with the features of its organization. A failed lookup is
// logged and reported as no features: the session itself is valid, and every paid operation
// checks its entitlement again.
func (s *server) toSessionInfo(ctx context.Context, i auth.SessionInfo) gen.SessionInfo {
	features := []string{}
	if s.entitlements != nil {
		f, err := s.entitlements.Features(ctx, i.Tenant.ID)
		if err != nil {
			s.log.ErrorContext(ctx, "entitlements lookup failed", "error", err)
		} else {
			features = f
		}
	}
	return gen.SessionInfo{
		Features:  features,
		User:      gen.User{Id: i.User.ID, Login: i.User.Login, DisplayName: i.User.DisplayName, IsPlatformAdmin: &i.User.IsPlatformAdmin},
		Tenant:    gen.Tenant{Id: i.Tenant.ID, Slug: i.Tenant.Slug, Name: i.Tenant.Name},
		Role:      gen.MembershipRole(i.Role),
		CsrfToken: i.CSRFToken,
		ExpiresAt: i.ExpiresAt,
	}
}
