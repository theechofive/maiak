package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

func TestPingLimits(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	l := newPingLimits(clk)
	limited := func(err error) bool {
		var rl *auth.RateLimitedError
		return errors.As(err, &rl)
	}

	// A known token: one ping per second, and known pings never count against the IP.
	for i := range 5 {
		if err := l.reserve("10.0.0.1", "token-a"); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
		l.knownToken("10.0.0.1")
		if err := l.reserve("10.0.0.1", "token-a"); !limited(err) {
			t.Fatalf("second ping within a second: %v", err)
		}
		clk.Advance(time.Second)
	}

	// Unknown tokens: never the per-token 429, but at most unknownPingLimit per IP and window.
	for i := range unknownPingLimit {
		if err := l.reserve("10.0.0.2", "unknown"); err != nil {
			t.Fatalf("unknown ping %d: %v", i, err)
		}
		l.unknownToken("unknown")
	}
	if err := l.reserve("10.0.0.2", "unknown"); !limited(err) {
		t.Fatalf("unknown ping over the IP limit: %v", err)
	}
	// Another IP and the known token of the first one are not affected.
	if err := l.reserve("10.0.0.3", "unknown"); err != nil {
		t.Fatalf("other IP: %v", err)
	}
	if err := l.reserve("10.0.0.1", "token-a"); err != nil {
		t.Fatalf("known token from another IP: %v", err)
	}
	clk.Advance(unknownPingWindow)
	if err := l.reserve("10.0.0.2", "unknown"); err != nil {
		t.Fatalf("after the window: %v", err)
	}

	// A failed lookup counts nowhere.
	if err := l.reserve("10.0.0.4", "token-b"); err != nil {
		t.Fatal(err)
	}
	l.failed("10.0.0.4", "token-b")
	if err := l.reserve("10.0.0.4", "token-b"); err != nil {
		t.Fatalf("retry after a failed lookup: %v", err)
	}
}

// Problems on the ping path never echo the path, which carries the secret token.
func TestHeartbeatProblemHidesToken(t *testing.T) {
	const secret = "s3cr3t-heartbeat-token"
	rec, p := do(t, newTestHandler(t), http.MethodGet, "/api/v1/heartbeat/"+secret+"/extra", "", nil)
	if rec.Code != http.StatusNotFound || p.Type != problemNotFound {
		t.Fatalf("code=%d problem=%+v", rec.Code, p)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("problem echoes the token: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"instance":"/api/v1/heartbeat/{token}"`) {
		t.Fatalf("instance not redacted: %s", rec.Body)
	}
}
