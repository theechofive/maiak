package api

import (
	"container/list"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/api/gen"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// The public ping endpoint is the only request path without a session (ADR-0092, ADR-0094,
// ADR-0096). Every token that is not known gets the same 404, malformed or not; the token and its
// hash are never logged or echoed.
const (
	// pingInterval: at most one ping per token per second reaches the database.
	pingInterval = time.Second
	// unknownPingLimit unknown (or malformed) tokens per unknownPingWindow per client IP get 404,
	// the rest 429; a misconfigured sender that keeps an old URL (one ping a minute) stays far
	// below it. Known tokens never count against it and are never refused by it.
	unknownPingLimit  = 30
	unknownPingWindow = 5 * time.Minute
	// pingBodyBytes is how much of an ignored body is read, so the connection can be reused.
	pingBodyBytes = 64 << 10
	// maxPingsInFlight bounds the lookups running or waiting for a database connection at once;
	// recentPingSlots of them are only for tokens accepted recently, so a flood of unknown
	// tokens cannot crowd out the senders that work.
	maxPingsInFlight = 64
	recentPingSlots  = 16
	// Bounds of the limiter tables; beyond them the oldest or arbitrary entries are dropped.
	pingTokenKeys    = 100_000
	pingIPKeys       = 10_000
	recentTokenSlots = 10_000
	// heartbeatTokenLen is the length of a token: 32 bytes in URL-safe base64 without padding.
	heartbeatTokenLen = 43
)

// Ping results (maiak_heartbeat_pings_total{result}).
const (
	pingAccepted    = "accepted"
	pingIgnored     = "ignored"
	pingUnknown     = "unknown"
	pingRateLimited = "rate_limited"
	pingBusy        = "busy"
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
		Help: "Heartbeat pings by result (accepted; ignored for paused or deleted heartbeats; unknown or malformed token, 404; " +
			"rate_limited: a token over one ping per second or an unknown token from a client IP over its limit, 429; " +
			"busy: too many pings in flight, 429; error).",
	}, []string{"result"})}
	for _, r := range []string{pingAccepted, pingIgnored, pingUnknown, pingRateLimited, pingBusy, pingError} {
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

// ping is ADR-0096: a malformed token is unknown without any further work; then the per-token
// limit, the in-flight bound and the lookup; only a token the lookup does not know is counted
// against the client IP, so unknown-token traffic from an address (or from the shared bucket of
// requests without a usable X-Forwarded-For) never blocks a known token.
func (s *server) ping(ctx context.Context, token string) error {
	l := s.pingLimits
	r := httpRequest(ctx)
	if !wellFormedHeartbeatToken(token) {
		return s.unknownPing(ctx, r)
	}
	hash := monitors.HashHeartbeatToken(token)
	key := string(hash)
	if ok, retry := l.perToken.Reserve(key); !ok {
		s.pings.pings.WithLabelValues(pingRateLimited).Inc()
		return &auth.RateLimitedError{RetryAfter: retry}
	}
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, pingBodyBytes))
	}
	release, ok := l.acquire(key)
	if !ok {
		l.perToken.Refund(key)
		s.pings.pings.WithLabelValues(pingBusy).Inc()
		return &auth.RateLimitedError{RetryAfter: time.Second}
	}
	defer release()
	res, err := s.monitors.Ping(ctx, hash)
	switch {
	case errors.Is(err, monitors.ErrUnknownHeartbeat):
		// Forgotten, so an unknown token never gets the per-token 429, only the per-IP one.
		l.perToken.Reset(key)
		l.recent.remove(key)
		return s.unknownPing(ctx, r)
	case err != nil:
		l.perToken.Refund(key)
		s.pings.pings.WithLabelValues(pingError).Inc()
		return err
	}
	l.recent.add(key)
	logging.SetTenantID(ctx, res.TenantID.String())
	if res.Recorded {
		s.pings.pings.WithLabelValues(pingAccepted).Inc()
	} else {
		s.pings.pings.WithLabelValues(pingIgnored).Inc()
	}
	return nil
}

// unknownPing answers a token that is not known: 404, or 429 once the client IP is over its
// unknown-token limit.
func (s *server) unknownPing(ctx context.Context, r *http.Request) error {
	ip := s.limiterIP(ctx, r).String()
	if ok, retry := s.pingLimits.perIP.Reserve(ip); !ok {
		s.pings.pings.WithLabelValues(pingRateLimited).Inc()
		return &auth.RateLimitedError{RetryAfter: retry}
	}
	s.pings.pings.WithLabelValues(pingUnknown).Inc()
	return errNotFound
}

// wellFormedHeartbeatToken reports whether token can be a token NewHeartbeatToken issued:
// exactly heartbeatTokenLen characters of the URL-safe base64 alphabet.
func wellFormedHeartbeatToken(token string) bool {
	if len(token) != heartbeatTokenLen {
		return false
	}
	for i := range len(token) {
		switch c := token[i]; {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// pingLimits are the in-memory limits of the ping endpoint (per api replica, ADR-0096), every
// table bounded: one ping per token per second, unknownPingLimit unknown tokens per client IP per
// unknownPingWindow, at most maxPingsInFlight lookups at once, and the recently accepted token
// hashes that may use the reserved in-flight slots.
type pingLimits struct {
	perToken *auth.Limiter
	perIP    *auth.Limiter
	recent   *recentTokens
	inFlight atomic.Int64
}

func newPingLimits(clk clock.Clock) *pingLimits {
	return &pingLimits{
		perToken: auth.NewLimiter(1, pingInterval, pingTokenKeys, clk),
		perIP:    auth.NewLimiter(unknownPingLimit, unknownPingWindow, pingIPKeys, clk),
		recent:   newRecentTokens(recentTokenSlots),
	}
}

// acquire takes an in-flight slot for a lookup of token hash key, or reports false when the
// slots are taken. Tokens accepted recently may use all slots, others all but recentPingSlots.
func (l *pingLimits) acquire(key string) (func(), bool) {
	limit := int64(maxPingsInFlight - recentPingSlots)
	if l.recent.contains(key) {
		limit = maxPingsInFlight
	}
	if l.inFlight.Add(1) > limit {
		l.inFlight.Add(-1)
		return nil, false
	}
	return func() { l.inFlight.Add(-1) }, true
}

// recentTokens is a bounded LRU set of token hashes.
type recentTokens struct {
	mu    sync.Mutex
	max   int
	order *list.List // front: most recently added
	index map[string]*list.Element
}

func newRecentTokens(maxKeys int) *recentTokens {
	return &recentTokens{max: maxKeys, order: list.New(), index: map[string]*list.Element{}}
}

func (r *recentTokens) contains(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.index[key]
	return ok
}

func (r *recentTokens) add(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.index[key]; ok {
		r.order.MoveToFront(e)
		return
	}
	r.index[key] = r.order.PushFront(key)
	if r.order.Len() > r.max {
		oldest := r.order.Back()
		r.order.Remove(oldest)
		delete(r.index, oldest.Value.(string))
	}
}

func (r *recentTokens) remove(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.index[key]; ok {
		r.order.Remove(e)
		delete(r.index, key)
	}
}

func (r *recentTokens) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order.Len()
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
