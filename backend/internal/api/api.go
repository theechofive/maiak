// Package api is the HTTP layer of cmd/api. It implements the server interface generated from
// api/openapi.yaml, validates every request against the spec, and maps domain errors to
// RFC 9457 problems in one place (errors.go).
package api

import (
	"context"
	"encoding/json"
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
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
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
	Auth    *auth.Service
	Logger  *slog.Logger
	Metrics *metrics.HTTP
}

type server struct {
	cfg  Config
	auth *auth.Service
	log  *slog.Logger
	// lastXFFWarn is the UnixNano time of the last missing X-Forwarded-For warning.
	lastXFFWarn atomic.Int64
}

var _ gen.ServerInterface = (*server)(nil)

// NewHandler builds the public HTTP handler.
func NewHandler(cfg Config, deps Deps) (http.Handler, error) {
	spec, err := gen.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("api: load embedded spec: %w", err)
	}
	s := &server{cfg: cfg, auth: deps.Auth, log: deps.Logger}

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.AccessLog(deps.Logger), httpx.Recover(deps.Logger))
	if deps.Metrics != nil {
		r.Use(deps.Metrics.Middleware)
	}
	r.Use(httpx.MaxBody(maxBodyBytes), withSessionSlot)
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

	return gen.HandlerWithOptions(s, gen.ChiServerOptions{
		BaseRouter: r,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, r, s.log, &badRequestError{detail: err.Error()})
		},
	}), nil
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
	})
	return slot.principal, slot.err
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

func (s *server) GetHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, gen.Health{Status: gen.Ok, Version: s.cfg.Version, Commit: s.cfg.Commit})
}

func (s *server) Login(w http.ResponseWriter, r *http.Request) {
	var body gen.LoginRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, r, s.log, &badRequestError{detail: "request body is not valid JSON for LoginRequest"})
		return
	}
	ip, trusted := httpx.ForwardedClientIP(r, s.cfg.TrustedProxyHops)
	if !trusted {
		// The peer is the proxy: limiting by it would let one client lock everybody out.
		// An invalid address makes auth use one shared bucket for such requests.
		s.warnMissingXFF(r.Context())
		ip = netip.Addr{}
	}
	res, err := s.auth.Login(r.Context(), auth.LoginInput{
		Email:     body.Email,
		Password:  body.Password,
		IP:        ip,
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	http.SetCookie(w, s.sessionCookie(res.Token, res.ExpiresAt))
	httpx.WriteJSON(w, http.StatusOK, toSessionInfo(res.SessionInfo))
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

func (s *server) Logout(w http.ResponseWriter, r *http.Request) {
	p, err := s.resolve(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.auth.Logout(r.Context(), p, httpx.ClientIP(r, s.cfg.TrustedProxyHops)); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	http.SetCookie(w, s.sessionCookie("", time.Time{}))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) GetMe(w http.ResponseWriter, r *http.Request) {
	p, err := s.resolve(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	info, err := s.auth.Me(r.Context(), p)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toSessionInfo(info))
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
