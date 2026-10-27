package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/notify"
)

func TestWriteErrorChannels(t *testing.T) {
	cases := []struct {
		err        error
		status     int
		typ        string
		retryAfter string
		detail     string
	}{
		{notify.ErrChannelNotFound, 404, problemNotFound, "", ""},
		{&notify.SendError{Kind: notify.KindRateLimited, Status: 429, RetryAfter: 2500 * time.Millisecond}, 429, problemRateLimited, "3", ""},
		{&notify.SendError{Kind: notify.KindRateLimited, Status: 429}, 429, problemRateLimited, "1", ""},
		{&notify.SendError{Kind: notify.KindNotFound, Status: 404, Permanent: true}, 502, problemSlackFailed, "", "removed or revoked"},
		{&notify.SendError{Kind: notify.KindHTTPStatus, Status: 500}, 502, problemSlackFailed, "", "HTTP 500"},
		{&notify.SendError{Kind: notify.KindTimeout}, 502, problemSlackFailed, "", "in time"},
		{&notify.SendError{Kind: notify.KindDNS}, 502, problemSlackFailed, "", "(dns)"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeError(rec, httptest.NewRequest(http.MethodPost, "/api/v1/channels/x/test", nil), discard(), c.err)
		var p problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if rec.Code != c.status || p.Type != c.typ || rec.Header().Get("Retry-After") != c.retryAfter || !strings.Contains(p.Detail, c.detail) {
			t.Errorf("%v: code=%d retry=%q problem=%+v", c.err, rec.Code, rec.Header().Get("Retry-After"), p)
		}
	}
}

// The channel resource never has a webhook field, and empty lists are [] rather than null.
func TestToChannelShape(t *testing.T) {
	b, _ := json.Marshal(toChannel(notify.Channel{ID: uuid.Nil, Name: "ops", Type: notify.TypeSlack, Events: []string{"down"},
		Mention: []string{}, Status: notify.StatusActive}))
	got := string(b)
	for _, want := range []string{`"events":["down"]`, `"mention":[]`, `"reminderMinutes":0`, `"status":"active"`, `"type":"slack_webhook"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "webhookUrl") || strings.Contains(got, "hooks.slack.com") {
		t.Fatalf("webhook in %s", got)
	}
}
