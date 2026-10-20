// Package httpx holds HTTP plumbing shared by all processes: problem+json and JSON writers,
// request ids, client IP extraction, access logs, panic recovery, the admin server and the
// graceful server lifecycle.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
)

// Problem is an RFC 9457 problem details object.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	// FieldErrors lists per-field validation failures (400 and 422 only).
	FieldErrors []FieldError `json:"fieldErrors,omitempty"`
}

// FieldError is one entry of Problem.FieldErrors.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// WriteProblem writes p as application/problem+json.
func WriteProblem(w http.ResponseWriter, p Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON writes v as application/json with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// RequestID assigns a request id (reusing a well-formed incoming X-Request-ID from the gateway),
// echoes it in the response and records it for logs. It also installs the request log fields,
// so it must be the outermost middleware.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := logging.WithFields(r.Context())
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			id = uuidv7.New().String()
		}
		logging.SetRequestID(ctx, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ClientIP returns the client address. With trustedHops > 0 it takes the entry of
// X-Forwarded-For appended by the outermost trusted proxy (counting from the right), so
// clients cannot spoof it by sending their own header; otherwise the TCP peer address.
// If the header is missing or invalid it falls back to the peer (see ForwardedClientIP).
func ClientIP(r *http.Request, trustedHops int) netip.Addr {
	a, _ := ForwardedClientIP(r, trustedHops)
	return a
}

// ForwardedClientIP is ClientIP that also reports whether the address is the client's. It is
// false when trustedHops > 0 but X-Forwarded-For is missing or invalid: the returned address
// is then the proxy (the TCP peer), which must not be used to tell clients apart.
func ForwardedClientIP(r *http.Request, trustedHops int) (netip.Addr, bool) {
	if trustedHops > 0 {
		var hops []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			for _, part := range strings.Split(v, ",") {
				if p := strings.TrimSpace(part); p != "" {
					hops = append(hops, p)
				}
			}
		}
		if len(hops) > 0 {
			i := max(len(hops)-trustedHops, 0)
			if a, err := netip.ParseAddr(hops[i]); err == nil {
				return a.Unmap(), true
			}
		}
	}
	return peerIP(r), trustedHops <= 0
}

func peerIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// AccessLog logs one line per request.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			level := slog.LevelInfo
			if sw.status >= 500 {
				level = slog.LevelError
			}
			log.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Int64("bytes", sw.bytes),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// Recover turns a panic into a 500 problem and logs it.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.ErrorContext(r.Context(), "panic in handler", "panic", fmt.Sprint(v))
					WriteProblem(w, Problem{
						Type: "urn:maiak:problem:internal", Title: "Internal server error",
						Status: http.StatusInternalServerError, RequestID: logging.RequestID(r.Context()),
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBody limits request bodies.
func MaxBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ReadyFunc reports whether the process can serve traffic.
type ReadyFunc func(ctx context.Context) error

// readyTimeout stays below the chart's readiness probe timeout (3 s).
const readyTimeout = 2 * time.Second

// AdminHandler serves /healthz (process alive), /readyz (dependencies reachable) and /metrics.
// It is bound to the admin port, which is never routed externally.
func AdminHandler(log *slog.Logger, ready ReadyFunc, gatherer prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := ready(ctx); err != nil {
			log.WarnContext(ctx, "readiness check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	return mux
}

// NewServer returns an http.Server with conservative timeouts.
func NewServer(addr string, h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// Serve runs the servers until ctx is cancelled (SIGTERM) or one of them fails, then shuts all
// of them down gracefully within shutdownTimeout.
func Serve(ctx context.Context, log *slog.Logger, shutdownTimeout time.Duration, servers ...*http.Server) error {
	g, gctx := errgroup.WithContext(ctx)
	for _, s := range servers {
		g.Go(func() error {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("server %s: %w", s.Addr, err)
			}
			return nil
		})
	}
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down", "timeout", shutdownTimeout)
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		var errs []error
		for _, s := range servers {
			if err := s.Shutdown(sctx); err != nil {
				errs = append(errs, fmt.Errorf("shutdown %s: %w", s.Addr, err))
			}
		}
		return errors.Join(errs...)
	})
	return g.Wait()
}
