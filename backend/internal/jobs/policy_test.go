package jobs

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Second, time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for attempts, w := range want {
		if got := Backoff(attempts); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, w)
		}
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		attempts int
		err      error
		want     outcome
	}{
		{"sent", 0, nil, outcome{status: statusSent}},
		{"first failure retries after 1s", 0, &notify.SendError{Kind: notify.KindTimeout},
			outcome{status: statusPending, errKind: "timeout", retryAt: now.Add(time.Second)}},
		{"second after 5s", 1, &notify.SendError{Kind: notify.KindHTTPStatus, Status: 500},
			outcome{status: statusPending, errKind: "http_status 500", retryAt: now.Add(5 * time.Second)}},
		{"fourth after 2m", 3, &notify.SendError{Kind: notify.KindConnect},
			outcome{status: statusPending, errKind: "connect", retryAt: now.Add(2 * time.Minute)}},
		{"fifth failure is final", 4, &notify.SendError{Kind: notify.KindTimeout},
			outcome{status: statusFailed, errKind: "timeout"}},
		{"retry-after longer than backoff wins", 0, &notify.SendError{Kind: notify.KindRateLimited, Status: 429, RetryAfter: 40 * time.Second},
			outcome{status: statusPending, errKind: "rate_limited", retryAt: now.Add(40 * time.Second)}},
		{"retry-after shorter than backoff", 2, &notify.SendError{Kind: notify.KindRateLimited, Status: 429, RetryAfter: time.Second},
			outcome{status: statusPending, errKind: "rate_limited", retryAt: now.Add(30 * time.Second)}},
		{"404 permanent, channel failed", 0, &notify.SendError{Kind: notify.KindNotFound, Status: 404, Permanent: true},
			outcome{status: statusFailed, errKind: "webhook_not_found", failChannel: true}},
		{"invalid url permanent, channel failed", 0, &notify.SendError{Kind: notify.KindInvalidURL, Permanent: true},
			outcome{status: statusFailed, errKind: "invalid_url", failChannel: true}},
		{"another permanent kind keeps the channel", 0, &notify.SendError{Kind: notify.KindOther, Permanent: true},
			outcome{status: statusFailed, errKind: "other"}},
		{"foreign error is other, never its text", 0, errors.New("dial tcp 10.0.0.1:443: https://hooks.slack.com/services/x"),
			outcome{status: statusPending, errKind: "other", retryAt: now.Add(time.Second)}},
	}
	for _, c := range cases {
		if got := decide(c.attempts, c.err, now); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestChannelLimiterSpacesMessagesPerChannel(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	l := newChannelLimiter(time.Second)
	a, b := uuid.New(), uuid.New()
	waits := []time.Duration{l.reserve(a, clk.Now()), l.reserve(a, clk.Now()), l.reserve(a, clk.Now()), l.reserve(b, clk.Now())}
	want := []time.Duration{0, time.Second, 2 * time.Second, 0}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
	clk.Advance(2500 * time.Millisecond)
	// The third message of a was booked for t+2s, so the next slot is t+3s: 500 ms from now.
	if w := l.reserve(a, clk.Now()); w != 500*time.Millisecond {
		t.Fatalf("after 2.5 s: wait %v", w)
	}
	clk.Advance(10 * time.Second)
	if w := l.reserve(a, clk.Now()); w != 0 {
		t.Fatalf("idle channel waits %v", w)
	}
}

func TestChannelLimiterPrunes(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	l := newChannelLimiter(time.Second)
	for range 1100 {
		l.reserve(uuid.New(), clk.Now())
	}
	clk.Advance(2 * time.Second)
	l.reserve(uuid.New(), clk.Now())
	if len(l.next) > 2 {
		t.Fatalf("limiter keeps %d idle channels", len(l.next))
	}
}
