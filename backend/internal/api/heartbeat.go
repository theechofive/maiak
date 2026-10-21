package api

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// The public ping endpoint is the only request path without a session (ADR-0092, ADR-0094). It
// hashes whatever token it gets and looks the hash up, so unknown tokens cost the same and get
// the same 404; the token and its hash are never logged or echoed.
const (
	// pingInterval: at most one accepted ping per token per second.
	pingInterval = time.Second
	// unknownPingLimit pings with unknown tokens per unknownPingWindow per client IP; a
	// misconfigured sender that keeps an old URL (one ping a minute) stays far below it.
	unknownPingLimit  = 30
	unknownPingWindow = 5 * time.Minute
	// pingBodyBytes is how much of an ignored body is read, so the connection can be reused.
	pingBodyBytes = 64 << 10
)

// Ping results (maiak_heartbeat_pings_total{result}).
const (
	pingAccepted    = "accepted"
	pingIgnored     = "ignored"
	pingUnknown     = "unknown"
	pingRateLimited = "rate_limited"
	pingError       = "error"
)

// PingMetrics counts heartbeat pings by result.
type PingMetrics struct {
	pings *prometheus.CounterVec
}

// NewPingMetrics registers the heartbeat ping counter.
func NewPingMetrics(reg prometheus.Registerer) *PingMetrics {
	m := &PingMetrics{pings: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "maiak_heartbeat_pings_total",
		Help: "Heartbeat pings by result (accepted, ignored for paused or deleted heartbeats, unknown token, rate_limited, error).",
	}, []string{"result"})}
	for _, r := range []string{pingAccepted, pingIgnored, pingUnknown, pingRateLimited, pingError} {
		m.pings.WithLabelValues(r)
	}
	reg.MustRegister(m.pings)
	return m
}

func (s *server) PingHeartbeat(ctx context.Context, req gen.PingHeartbeatRequestObject) (gen.PingHeartbeatResponseObject, error) {
	if err := s.ping(ctx, req.Token); err != nil {
		return nil, err
	}
	return gen.PingHeartbeat204Response{}, nil
}

func (s *server) PingHeartbeatPost(ctx context.Context, req gen.PingHeartbeatPostRequestObject) (gen.PingHeartbeatPostResponseObject, error) {
	if err := s.ping(ctx, req.Token); err != nil {
		return nil, err
	}
	return gen.PingHeartbeatPost204Response{}, nil
}

func (s *server) ping(ctx context.Context, token string) error {
	r := httpRequest(ctx)
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, pingBodyBytes))
	}
	hash := monitors.HashHeartbeatToken(token)
	ip, key := s.limiterIP(ctx, r).String(), string(hash)
	if err := s.pingLimits.reserve(ip, key); err != nil {
		s.pings.pings.WithLabelValues(pingRateLimited).Inc()
		return err
	}
	res, err := s.monitors.Ping(ctx, hash)
	switch {
	case errors.Is(err, monitors.ErrUnknownHeartbeat):
		s.pingLimits.unknownToken(key)
		s.pings.pings.WithLabelValues(pingUnknown).Inc()
		return errNotFound
	case err != nil:
		s.pingLimits.failed(ip, key)
		s.pings.pings.WithLabelValues(pingError).Inc()
		return err
	}
	s.pingLimits.knownToken(ip)
	logging.SetTenantID(ctx, res.TenantID.String())
	if res.Recorded {
		s.pings.pings.WithLabelValues(pingAccepted).Inc()
	} else {
		s.pings.pings.WithLabelValues(pingIgnored).Inc()
	}
	return nil
}

// pingLimits are the in-memory limits of the ping endpoint (per api replica, ADR-0094): one
// accepted ping per token per second, and unknownPingLimit pings with unknown tokens per client
// IP per unknownPingWindow. An attempt is counted for both before the lookup, and settled after.
type pingLimits struct {
	perToken *auth.Limiter
	perIP    *auth.Limiter
}

func newPingLimits(clk clock.Clock) pingLimits {
	return pingLimits{
		perToken: auth.NewLimiter(1, pingInterval, 100_000, clk),
		perIP:    auth.NewLimiter(unknownPingLimit, unknownPingWindow, 10_000, clk),
	}
}

// reserve counts a ping of token hash key from ip, or returns a *auth.RateLimitedError.
func (l pingLimits) reserve(ip, key string) error {
	if ok, retry := l.perIP.Reserve(ip); !ok {
		return &auth.RateLimitedError{RetryAfter: retry}
	}
	if ok, retry := l.perToken.Reserve(key); !ok {
		l.perIP.Refund(ip)
		return &auth.RateLimitedError{RetryAfter: retry}
	}
	return nil
}

// knownToken settles a ping of a known token: it does not count against the IP.
func (l pingLimits) knownToken(ip string) { l.perIP.Refund(ip) }

// unknownToken settles a ping of an unknown token: it stays counted against the IP, and the
// token is forgotten, so every unknown token keeps getting the same 404 and never this 429.
func (l pingLimits) unknownToken(key string) { l.perToken.Reset(key) }

// failed settles a ping that could not be looked up: it counts nowhere.
func (l pingLimits) failed(ip, key string) {
	l.perToken.Refund(key)
	l.perIP.Refund(ip)
}

func (s *server) RegenerateHeartbeatToken(ctx context.Context, req gen.RegenerateHeartbeatTokenRequestObject) (gen.RegenerateHeartbeatTokenResponseObject, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	token, err := s.monitors.RegenerateHeartbeatToken(ctx, a, req.MonitorId)
	if err != nil {
		return nil, err
	}
	return gen.RegenerateHeartbeatToken200JSONResponse{HeartbeatUrl: s.heartbeatURL(token)}, nil
}

// heartbeatURL is the ping URL of a token: MAIAK_PUBLIC_URL, the ping path and the token.
func (s *server) heartbeatURL(token string) string {
	return s.cfg.PublicURL + monitors.HeartbeatPath + token
}
