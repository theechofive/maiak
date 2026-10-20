// Package logging builds the JSON slog logger and carries request-scoped fields (request_id,
// tenant_id) through the context so every log line of a request can be correlated.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// ParseLevel parses debug, info, warn or error.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
}

// New returns a JSON logger writing to w that adds request fields from the context.
func New(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(contextHandler{h})
}

// Fields are the request-scoped log attributes. They are mutable because the tenant becomes
// known only after authentication, deep inside the handler chain.
type Fields struct {
	mu        sync.Mutex
	requestID string
	tenantID  string
}

type fieldsKey struct{}

// WithFields returns a context carrying a new, empty Fields.
func WithFields(ctx context.Context) (context.Context, *Fields) {
	f := &Fields{}
	return context.WithValue(ctx, fieldsKey{}, f), f
}

func fieldsFrom(ctx context.Context) *Fields {
	f, _ := ctx.Value(fieldsKey{}).(*Fields)
	return f
}

// SetRequestID records the request id for log lines of this request.
func SetRequestID(ctx context.Context, id string) {
	if f := fieldsFrom(ctx); f != nil {
		f.mu.Lock()
		f.requestID = id
		f.mu.Unlock()
	}
}

// SetTenantID records the tenant id for log lines of this request.
func SetTenantID(ctx context.Context, id string) {
	if f := fieldsFrom(ctx); f != nil {
		f.mu.Lock()
		f.tenantID = id
		f.mu.Unlock()
	}
}

// RequestID returns the request id recorded in ctx, or "".
func RequestID(ctx context.Context) string {
	f := fieldsFrom(ctx)
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestID
}

type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if f := fieldsFrom(ctx); f != nil {
		f.mu.Lock()
		if f.requestID != "" {
			r.AddAttrs(slog.String("request_id", f.requestID))
		}
		if f.tenantID != "" {
			r.AddAttrs(slog.String("tenant_id", f.tenantID))
		}
		f.mu.Unlock()
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
