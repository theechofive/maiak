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
	middleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/queries"
)

// SessionCookie is the name of the session cookie.
const SessionCookie = "maiak_session"

const (
	csrfHeader   = "X-CSRF-Token"
	maxBodyBytes = 64 << 10
	// xffWarnInterval rate-limits the missing X-Forwarded-For warning.
	xffWarnInterval = time.Minute
)

// Config holds the HTTP-layer settings.
type Config struct {
	Version          string
	Commit           string
	CookieSecure     bool
	TrustedProxyHops int
}

// Deps are the collaborators of the HTTP layer.
type Deps struct {
	Auth     *auth.Service
	Monitors *monitors.Service
	Queries  *queries.Service
	Slack    *notify.Service
	Logger   *slog.Logger
	Metrics  *metrics.HTTP
}

type server struct {
	// unimplemented answers 501 for every operation this type does not implement yet. Removing
	// an operation from it without implementing it on server is a compile error (see below).
	unimplemented

	cfg      Config
	auth     *auth.Service
	monitors *monitors.Service
	queries  *queries.Service
	slack    *notify.Service
	log      *slog.Logger
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
	s := &server{cfg: cfg, auth: deps.Auth, monitors: deps.Monitors, queries: deps.Queries, slack: deps.Slack, log: deps.Logger}

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.AccessLog(deps.Logger), httpx.Recover(deps.Logger))
	if deps.Metrics != nil {
		r.Use(deps.Metrics.Middleware)
	}
	r.Use(noStore, httpx.MaxBody(maxBodyBytes), withSessionSlot)
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
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { writeError(w, r, s.log, errNotFound) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { writeError(w, r, s.log, errMethodNotAllowed) })

	badRequest := func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, r, s.log, &badRequestError{detail: err.Error()})
	}
	strict := gen.NewStrictHandlerWithOptions(s, []gen.StrictMiddlewareFunc{withHTTPRequest}, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  badRequest,
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) { writeError(w, r, s.log, err) },
	})
	return gen.HandlerWithOptions(strict, gen.ChiServerOptions{
		BaseRouter:       r,
		ErrorHandlerFunc: badRequest,
	}), nil
}

// noStore keeps API responses (sessions, CSRF tokens, tenant data) out of shared caches.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
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

func (s *server) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	r := httpRequest(ctx)
	ip, trusted := httpx.ForwardedClientIP(r, s.cfg.TrustedProxyHops)
	if !trusted {
		// The peer is the proxy: limiting by it would let one client lock everybody out.
		// An invalid address makes auth use one shared bucket for such requests.
		s.warnMissingXFF(ctx)
		ip = netip.Addr{}
	}
	res, err := s.auth.Login(ctx, auth.LoginInput{
		Email:     req.Body.Email,
		Password:  req.Body.Password,
		IP:        ip,
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		return nil, err
	}
	logging.SetTenantID(ctx, res.Tenant.ID.String())
	cookie := s.sessionCookie(res.Token, res.ExpiresAt).String()
	return gen.Login200JSONResponse{
		Body:    toSessionInfo(res.SessionInfo),
		Headers: gen.Login200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func (s *server) warnMissingXFF(ctx context.Context) {
	now := time.Now().UnixNano()
	last := s.lastXFFWarn.Load()
	if now-last < int64(xffWarnInterval) || !s.lastXFFWarn.CompareAndSwap(last, now) {
		return
	}
	s.log.WarnContext(ctx, "login without a valid X-Forwarded-For although MAIAK_TRUSTED_PROXY_HOPS > 0; rate limiting it under a shared key",
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
	return gen.GetMe200JSONResponse(toSessionInfo(info)), nil
}

// sessionCookie builds the session cookie; an empty token expires it.
func (s *server) sessionCookie(token string, expires time.Time) *http.Cookie {
	c := &http.Cookie{
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

func toSessionInfo(i auth.SessionInfo) gen.SessionInfo {
	return gen.SessionInfo{
		User:      gen.User{Id: i.User.ID, Email: i.User.Email, DisplayName: i.User.DisplayName},
		Tenant:    gen.Tenant{Id: i.Tenant.ID, Slug: i.Tenant.Slug, Name: i.Tenant.Name},
		Role:      gen.MembershipRole(i.Role),
		CsrfToken: i.CSRFToken,
		ExpiresAt: i.ExpiresAt,
	}
}
