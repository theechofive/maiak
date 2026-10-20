package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const goodURL = "https://hooks.slack.com/services/T0000/B0000/XXXXsecretXXXX"

func TestValidateWebhookURL(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{goodURL, true},
		{"https://hooks.slack.com/services/T0-0_0/B/x", true},
		{"", false},
		{"http://hooks.slack.com/services/T0/B0/x", false},
		{"https://hooks.slack.com/services/", false},
		{"https://hooks.slack.com/workflows/T0/B0/x", false},
		{"https://hooks.slack.com:443/services/T0/B0/x", false},
		{"https://hooks.slack.com:8443/services/T0/B0/x", false},
		{"https://user@hooks.slack.com/services/T0/B0/x", false},
		{"https://hooks.slack.com.evil.example/services/T0/B0/x", false},
		{"https://evil.example/?https://hooks.slack.com/services/T0/B0/x", false},
		{"https://hooks.slack.com/services/T0/B0/x?redirect=1", false},
		{"https://hooks.slack.com/services/T0/B0/x#frag", false},
		{"https://hooks.slack.com/services/T0/B0/../../x", false},
		{"https://hooks.slack.com/services/T0/B0/x y", false},
		{"https://hooks.slack.com/services/T0/B0/x\n", false},
		{"https://HOOKS.SLACK.COM/services/T0/B0/x", false},
		{" https://hooks.slack.com/services/T0/B0/x", false},
		{"https://hooks.slack.com/services/" + strings.Repeat("a", MaxWebhookURLLength), false},
		{"https://hooks.slack.com/services/" + strings.Repeat("a", MaxWebhookURLLength-len("https://hooks.slack.com/services/")), true},
	}
	for _, c := range cases {
		msg := ValidateWebhookURL(c.url)
		if (msg == "") != c.ok {
			t.Errorf("%q: message %q, want ok=%v", c.url, msg, c.ok)
		}
		if strings.Contains(msg, "T0") || strings.Contains(msg, "evil") || strings.Contains(msg, "aaaa") {
			t.Errorf("message echoes the URL: %q", msg)
		}
	}
}

// rewriteDoer sends every request to a local test server instead of hooks.slack.com and records
// what the notifier asked for. The production guard refuses loopback (L-041).
type rewriteDoer struct {
	target *url.URL
	client *http.Client
	mu     sync.Mutex
	urls   []string
	bodies []string
}

func (d *rewriteDoer) Do(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	d.mu.Lock()
	d.urls = append(d.urls, req.URL.String())
	d.bodies = append(d.bodies, string(b))
	d.mu.Unlock()
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = d.target.Scheme, d.target.Host, ""
	out.Body = io.NopCloser(strings.NewReader(string(b)))
	out.RequestURI = ""
	return d.client.Do(out)
}

func newRewrite(t *testing.T, h http.HandlerFunc, timeout time.Duration) *rewriteDoer {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &rewriteDoer{target: u, client: &http.Client{Timeout: timeout}}
}

func TestSendSuccess(t *testing.T) {
	var gotCT, gotMethod string
	d := newRewrite(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT, gotMethod = r.Header.Get("Content-Type"), r.Method
		_, _ = io.WriteString(w, "ok")
	}, time.Second)
	if err := NewSlackNotifier(d).Send(context.Background(), goodURL, "hello <world>"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotCT != "application/json" || len(d.urls) != 1 || d.urls[0] != goodURL {
		t.Fatalf("method=%s ct=%s urls=%v", gotMethod, gotCT, d.urls)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(d.bodies[0]), &body); err != nil || body["text"] != "hello <world>" {
		t.Fatalf("body = %s", d.bodies[0])
	}
}

func TestSendStatuses(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		status     int
		retryAfter string
		kind       string
		permanent  bool
		wantAfter  time.Duration
	}{
		{"404 revoked", 404, "", KindNotFound, true, 0},
		{"410 gone", 410, "", KindNotFound, true, 0},
		{"429 seconds", 429, "7", KindRateLimited, false, 7 * time.Second},
		{"429 date", 429, now.Add(90 * time.Second).Format(http.TimeFormat), KindRateLimited, false, 90 * time.Second},
		{"429 huge", 429, "999999", KindRateLimited, false, time.Hour},
		{"429 garbage", 429, "soon", KindRateLimited, false, 0},
		{"429 none", 429, "", KindRateLimited, false, 0},
		{"500", 500, "", KindHTTPStatus, false, 0},
		{"400 invalid payload", 400, "", KindHTTPStatus, false, 0},
		{"403", 403, "", KindHTTPStatus, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newRewrite(t, func(w http.ResponseWriter, _ *http.Request) {
				if c.retryAfter != "" {
					w.Header().Set("Retry-After", c.retryAfter)
				}
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, "no_service secret-body-from-slack")
			}, time.Second)
			n := NewSlackNotifier(d)
			n.now = func() time.Time { return now }
			err := n.Send(context.Background(), goodURL, "x")
			var se *SendError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *SendError", err)
			}
			if se.Kind != c.kind || se.Permanent != c.permanent || se.Status != c.status || se.RetryAfter != c.wantAfter {
				t.Fatalf("got %+v", se)
			}
			if strings.Contains(err.Error(), "secret-body") || strings.Contains(err.Error(), "hooks.slack.com") {
				t.Fatalf("error leaks: %v", err)
			}
		})
	}
}

func TestSendTimeoutAndNetworkErrorsAreKinds(t *testing.T) {
	block := make(chan struct{})
	d := newRewrite(t, func(http.ResponseWriter, *http.Request) { <-block }, 50*time.Millisecond)
	// Registered after the server's Close, so it runs first and unblocks the handler.
	t.Cleanup(func() { close(block) })
	err := NewSlackNotifier(d).Send(context.Background(), goodURL, "x")
	var se *SendError
	if !errors.As(err, &se) || se.Kind != KindTimeout || se.Permanent {
		t.Fatalf("timeout: %v", err)
	}

	// A refused connection: the raw error names the address and the URL; the SendError must not.
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL)
	srv.Close()
	d = &rewriteDoer{target: u, client: &http.Client{Timeout: time.Second}}
	err = NewSlackNotifier(d).Send(context.Background(), goodURL, "x")
	if !errors.As(err, &se) || se.Permanent || se.Kind == "" {
		t.Fatalf("refused: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "XXXXsecret") {
		t.Fatalf("error leaks the address or URL: %v", err)
	}
}

func TestSendRevalidatesURL(t *testing.T) {
	d := newRewrite(t, func(http.ResponseWriter, *http.Request) { t.Error("request sent") }, time.Second)
	for _, bad := range []string{"https://evil.example/services/T0/B0/x", "http://hooks.slack.com/services/T0/B0/x", ""} {
		err := NewSlackNotifier(d).Send(context.Background(), bad, "x")
		var se *SendError
		if !errors.As(err, &se) || se.Kind != KindInvalidURL || !se.Permanent {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":   0,
		"0":  0,
		"-5": 0,
		"30": 30 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat): 0,
	} {
		if got := parseRetryAfter(v, now); got != want {
			t.Errorf("%q: %v, want %v", v, got, want)
		}
	}
}
