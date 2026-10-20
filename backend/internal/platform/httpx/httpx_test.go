package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    []string
		hops   int
		want   string
		// trusted is ForwardedClientIP's second result.
		trusted bool
	}{
		{"no proxy trust ignores header", "10.0.0.1:1234", []string{"1.2.3.4"}, 0, "10.0.0.1", true},
		{"one hop takes rightmost", "10.0.0.1:1234", []string{"6.6.6.6, 1.2.3.4"}, 1, "1.2.3.4", true},
		{"two hops", "10.0.0.1:1234", []string{"6.6.6.6, 1.2.3.4, 10.0.0.9"}, 2, "1.2.3.4", true},
		{"multiple header lines", "10.0.0.1:1234", []string{"6.6.6.6", "1.2.3.4"}, 1, "1.2.3.4", true},
		{"fewer entries than hops", "10.0.0.1:1234", []string{"1.2.3.4"}, 3, "1.2.3.4", true},
		{"missing header falls back to peer", "10.0.0.1:1234", nil, 1, "10.0.0.1", false},
		{"garbage falls back to peer", "10.0.0.1:1234", []string{"not-an-ip"}, 1, "10.0.0.1", false},
		{"ipv6 peer", "[2001:db8::1]:443", nil, 0, "2001:db8::1", true},
		{"mapped ipv4 is unmapped", "[::ffff:10.0.0.1]:443", nil, 0, "10.0.0.1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remote
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r, c.hops).String(); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
			if got, trusted := ForwardedClientIP(r, c.hops); got.String() != c.want || trusted != c.trusted {
				t.Fatalf("ForwardedClientIP = %s, %v; want %s, %v", got, trusted, c.want, c.trusted)
			}
		})
	}
}

func TestWriteProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, Problem{Type: "urn:maiak:problem:x", Title: "X", Status: 418, RequestID: "r1"})
	if rec.Code != 418 || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("code=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p["type"] != "urn:maiak:problem:x" || p["status"] != float64(418) || p["request_id"] != "r1" {
		t.Fatalf("body = %v", p)
	}
	if _, ok := p["detail"]; ok {
		t.Fatal("empty detail should be omitted")
	}
}

func TestRequestIDReusesWellFormedHeader(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Request-ID")
	}))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "abc-123")
	h.ServeHTTP(rec, r)
	if rec.Header().Get("X-Request-ID") != "abc-123" || seen != "abc-123" {
		t.Fatalf("got %q", rec.Header().Get("X-Request-ID"))
	}

	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "bad id with spaces")
	h.ServeHTTP(rec, r)
	if got := rec.Header().Get("X-Request-ID"); got == "" || got == "bad id with spaces" {
		t.Fatalf("malformed id not replaced: %q", got)
	}
}

func TestAdminHandler(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	readyErr := errors.New("db down")
	var fail bool
	h := AdminHandler(log, func(context.Context) error {
		if fail {
			return readyErr
		}
		return nil
	}, prometheus.NewRegistry())

	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/metrics": 200, "/nope": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: code %d, want %d", path, rec.Code, want)
		}
	}
	fail = true
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with failing dependency: %d", rec.Code)
	}
}

func TestAccessLogUsesRouteTemplate(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelInfo)
	r := chi.NewRouter()
	r.Use(RequestID, AccessLog(log))
	r.Get("/api/v1/monitors/{monitorId}", func(w http.ResponseWriter, r *http.Request) {
		logging.SetTenantID(r.Context(), "tenant-1")
		w.WriteHeader(http.StatusTeapot)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitors/0192-secret-id?token=s3cr3t", nil))
	id := rec.Header().Get("X-Request-Id")
	var e map[string]any
	if err := json.Unmarshal(buf.Bytes(), &e); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	if e["route"] != "/api/v1/monitors/{monitorId}" || e["method"] != "GET" || e["status"] != float64(http.StatusTeapot) ||
		e["request_id"] != id || id == "" || e["tenant_id"] != "tenant-1" || e["duration_ms"] == nil {
		t.Fatalf("access log = %v", e)
	}
	if strings.Contains(buf.String(), "secret") {
		t.Fatalf("access log contains the raw path or query: %s", buf.String())
	}
}
