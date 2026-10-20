package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestHandler builds the handler without a database; only paths that fail before reaching
// the auth service can be exercised (the login flow is covered by the integration test).
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	h, err := NewHandler(Config{Version: "v1", Commit: "abc"}, Deps{Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type problem struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Status      int    `json:"status"`
	Detail      string `json:"detail"`
	RequestID   string `json:"request_id"`
	FieldErrors []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"fieldErrors"`
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) (*httptest.ResponseRecorder, problem) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var p problem
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("problem body: %v", err)
		}
	}
	return rec, p
}

func TestHealth(t *testing.T) {
	rec, _ := do(t, newTestHandler(t), http.MethodGet, "/api/v1/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ok" || body["version"] != "v1" || body["commit"] != "abc" {
		t.Fatalf("body = %v", body)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

func TestUnknownRouteIsProblem(t *testing.T) {
	rec, p := do(t, newTestHandler(t), http.MethodGet, "/api/v1/nope", "", nil)
	if rec.Code != http.StatusNotFound || p.Type != problemNotFound || p.RequestID == "" {
		t.Fatalf("code=%d problem=%+v", rec.Code, p)
	}
	rec, p = do(t, newTestHandler(t), http.MethodDelete, "/api/v1/health", "", nil)
	if rec.Code != http.StatusMethodNotAllowed || p.Type != problemMethodNotAllowed {
		t.Fatalf("code=%d problem=%+v", rec.Code, p)
	}
}

func TestSecuredEndpointsNeedSession(t *testing.T) {
	h := newTestHandler(t)
	rec, p := do(t, h, http.MethodGet, "/api/v1/auth/me", "", nil)
	if rec.Code != http.StatusUnauthorized || p.Type != problemUnauthenticated {
		t.Fatalf("me: code=%d problem=%+v", rec.Code, p)
	}
	rec, p = do(t, h, http.MethodPost, "/api/v1/auth/logout", "", nil)
	if rec.Code != http.StatusUnauthorized || p.Type != problemUnauthenticated {
		t.Fatalf("logout: code=%d problem=%+v", rec.Code, p)
	}
}

func TestLoginValidationDoesNotEchoPassword(t *testing.T) {
	h := newTestHandler(t)
	secret := strings.Repeat("s3cr3t", 200) // longer than maxLength 1024
	rec, p := do(t, h, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":"a@example.com","password":%q}`, secret), map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusBadRequest || p.Type != problemBadRequest {
		t.Fatalf("code=%d problem=%+v", rec.Code, p)
	}
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Fatal("validation error echoes the password")
	}
	if len(p.FieldErrors) != 1 || p.FieldErrors[0].Field != "password" {
		t.Fatalf("field errors = %+v, want one for password", p.FieldErrors)
	}

	for name, body := range map[string]string{
		"missing field": `{"email":"a@example.com"}`,
		"unknown field": `{"email":"a@example.com","password":"x","admin":true}`,
		"not json":      `email=a`,
	} {
		rec, p := do(t, h, http.MethodPost, "/api/v1/auth/login", body, map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusBadRequest || p.Type != problemBadRequest {
			t.Errorf("%s: code=%d problem=%+v", name, rec.Code, p)
		}
	}
	rec, _ = do(t, h, http.MethodPost, "/api/v1/auth/login", `{"email":"a@example.com","password":"x"}`, map[string]string{"Content-Type": "text/plain"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("wrong content type: code=%d", rec.Code)
	}
}

func TestWriteErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		typ    string
	}{
		{auth.ErrInvalidCredentials, 401, problemInvalidCredentials},
		{fmt.Errorf("wrapped: %w", auth.ErrUnauthenticated), 401, problemUnauthenticated},
		{auth.ErrCSRF, 403, problemCSRF},
		{&auth.RateLimitedError{RetryAfter: 1500 * time.Millisecond}, 429, problemRateLimited},
		{&badRequestError{detail: "x"}, 400, problemBadRequest},
		{errNotFound, 404, problemNotFound},
		{errNotImplemented, 501, problemNotImplemented},
		{&validation.Error{Fields: []validation.FieldError{{Field: "url", Message: "must use https"}}}, 422, problemValidation},
		{errors.New("database exploded: password=hunter2"), 500, problemInternal},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		writeError(rec, req, discard(), c.err)
		var p problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if rec.Code != c.status || p.Status != c.status || p.Type != c.typ {
			t.Errorf("%v: code=%d problem=%+v", c.err, rec.Code, p)
		}
		if rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%v: content type %q", c.err, rec.Header().Get("Content-Type"))
		}
		if c.status == 500 && (strings.Contains(rec.Body.String(), "hunter2") || p.Detail != "") {
			t.Errorf("internal error leaks detail: %s", rec.Body)
		}
		if c.status == 422 && (len(p.FieldErrors) != 1 || p.FieldErrors[0].Field != "url" || p.FieldErrors[0].Message != "must use https") {
			t.Errorf("422 field errors = %+v", p.FieldErrors)
		}
		if c.status == 429 && rec.Header().Get("Retry-After") != "2" {
			t.Errorf("Retry-After = %q, want 2", rec.Header().Get("Retry-After"))
		}
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	s := &server{cfg: Config{CookieSecure: true}}
	c := s.sessionCookie("tok", time.Now().Add(time.Hour))
	if c.Name != SessionCookie || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("cookie = %+v", c)
	}
	cleared := s.sessionCookie("", time.Time{})
	if cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("cleared cookie = %+v", cleared)
	}
}

func TestMissingXFFWarningIsRateLimited(t *testing.T) {
	var buf strings.Builder
	s := &server{cfg: Config{TrustedProxyHops: 1}, log: slog.New(slog.NewTextHandler(&buf, nil))}
	for i := 0; i < 5; i++ {
		s.warnMissingXFF(context.Background())
	}
	if n := strings.Count(buf.String(), "X-Forwarded-For"); n != 1 {
		t.Fatalf("%d warnings logged, want 1:\n%s", n, buf.String())
	}
	s.lastXFFWarn.Store(time.Now().Add(-2 * xffWarnInterval).UnixNano())
	s.warnMissingXFF(context.Background())
	if n := strings.Count(buf.String(), "X-Forwarded-For"); n != 2 {
		t.Fatalf("warning not repeated after the interval: %d", n)
	}
}
