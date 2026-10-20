package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/notify"
)

func TestWriteErrorSlack(t *testing.T) {
	cases := []struct {
		err        error
		status     int
		typ        string
		retryAfter string
		detail     string
	}{
		{notify.ErrNotConfigured, 409, problemSlackNotConfigured, "", ""},
		{&notify.SendError{Kind: notify.KindRateLimited, Status: 429, RetryAfter: 2500 * time.Millisecond}, 429, problemRateLimited, "3", ""},
		{&notify.SendError{Kind: notify.KindRateLimited, Status: 429}, 429, problemRateLimited, "1", ""},
		{&notify.SendError{Kind: notify.KindNotFound, Status: 404, Permanent: true}, 502, problemSlackFailed, "", "removed or revoked"},
		{&notify.SendError{Kind: notify.KindHTTPStatus, Status: 500}, 502, problemSlackFailed, "", "HTTP 500"},
		{&notify.SendError{Kind: notify.KindTimeout}, 502, problemSlackFailed, "", "in time"},
		{&notify.SendError{Kind: notify.KindDNS}, 502, problemSlackFailed, "", "(dns)"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeError(rec, httptest.NewRequest(http.MethodPost, "/api/v1/settings/slack/test", nil), discard(), c.err)
		var p problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if rec.Code != c.status || p.Type != c.typ || rec.Header().Get("Retry-After") != c.retryAfter || !strings.Contains(p.Detail, c.detail) {
			t.Errorf("%v: code=%d retry=%q problem=%+v", c.err, rec.Code, rec.Header().Get("Retry-After"), p)
		}
	}
}

func TestToSlackSettingsOmitsStatusWhenUnconfigured(t *testing.T) {
	b, _ := json.Marshal(toSlackSettings(notify.Settings{}))
	if string(b) != `{"configured":false}` {
		t.Fatalf("got %s", b)
	}
	b, _ = json.Marshal(toSlackSettings(notify.Settings{Configured: true, Status: notify.StatusFailed}))
	if string(b) != `{"configured":true,"status":"failed"}` {
		t.Fatalf("got %s", b)
	}
}
