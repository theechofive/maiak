package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

func TestWellFormedHeartbeatToken(t *testing.T) {
	for range 20 {
		token, _, err := monitors.NewHeartbeatToken()
		if err != nil {
			t.Fatal(err)
		}
		if !wellFormedHeartbeatToken(token) {
			t.Fatalf("issued token %q is not well formed", token)
		}
	}
	ok := strings.Repeat("a", 42)
	for _, bad := range []string{"", "x", ok, ok + "aa", ok + "=", ok + "+", ok + "/", ok + ".", ok + "%", ok + "é"[:1], " " + ok} {
		if wellFormedHeartbeatToken(bad) {
			t.Errorf("%q is well formed", bad)
		}
	}
	if !wellFormedHeartbeatToken(ok+"-") || !wellFormedHeartbeatToken(ok+"_") || !wellFormedHeartbeatToken(strings.Repeat("Z9", 21)+"0") {
		t.Error("valid alphabet refused")
	}
}

// readSpy records whether the handler read the body.
type readSpy struct {
	mu   sync.Mutex
	read bool
}

func (r *readSpy) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.read = true
	return 0, fmt.Errorf("body read")
}

func (r *readSpy) wasRead() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read
}

// The handler below has no monitors service: a lookup would panic and answer 500, so every 404
// and 429 here was decided without the database.
func TestPingMalformedTokenNeverReachesDatabase(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	h, err := NewHandler(Config{Version: "v1", Commit: "abc"}, Deps{Logger: discard(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ping := func(method, token string) (*httptest.ResponseRecorder, *readSpy) {
		t.Helper()
		spy := &readSpy{}
		req := httptest.NewRequest(method, monitors.HeartbeatPath+token, spy)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, spy
	}
	ok := strings.Repeat("a", 42)
	bodies := map[string]bool{}
	malformed := []string{"x", ok, ok + "aa", ok + "%3D", ok + ".", "%2e%2e"}
	for i := range unknownPingLimit {
		method := http.MethodGet
		if i%2 == 1 {
			method = http.MethodPost
		}
		rec, spy := ping(method, malformed[i%len(malformed)])
		if rec.Code != http.StatusNotFound {
			t.Fatalf("malformed %d: %d %s", i, rec.Code, rec.Body)
		}
		if spy.wasRead() {
			t.Fatalf("malformed %d: body read", i)
		}
		var p problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		p.RequestID = ""
		bodies[fmt.Sprintf("%+v %s", p, rec.Header().Get("Content-Type"))] = true
	}
	if len(bodies) != 1 {
		t.Fatalf("malformed tokens answered differently: %v", bodies)
	}
	// Requests without X-Forwarded-For share one bucket, which is now exhausted for unknown tokens.
	if rec, _ := ping(http.MethodGet, "x"); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d %v", rec.Code, rec.Header())
	}
	// A well-formed token is not refused by that bucket: it goes on to the lookup (500 here).
	if rec, _ := ping(http.MethodGet, ok+"b"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("well-formed token from the exhausted bucket: %d %s", rec.Code, rec.Body)
	}
}

func TestPingLimitsInFlight(t *testing.T) {
	l := newPingLimits(clock.NewFake(time.Now()))
	l.recent.add("known")
	var releases []func()
	for i := range maxPingsInFlight - recentPingSlots {
		release, ok := l.acquire(fmt.Sprintf("other-%d", i))
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		releases = append(releases, release)
	}
	if _, ok := l.acquire("other-x"); ok {
		t.Fatal("unknown token got a reserved slot")
	}
	// Recently accepted tokens still get the reserved slots, and no more.
	for i := range recentPingSlots {
		release, ok := l.acquire("known")
		if !ok {
			t.Fatalf("reserved slot %d refused", i)
		}
		releases = append(releases, release)
	}
	if _, ok := l.acquire("known"); ok {
		t.Fatal("more than maxPingsInFlight")
	}
	for _, r := range releases {
		r()
	}
	if n := l.inFlight.Load(); n != 0 {
		t.Fatalf("in flight after release: %d", n)
	}
	if _, ok := l.acquire("other-x"); !ok {
		t.Fatal("slot not released")
	}
}

func TestRecentTokensBounded(t *testing.T) {
	r := newRecentTokens(3)
	for _, k := range []string{"a", "b", "c", "a", "d"} {
		r.add(k)
	}
	// b was the least recently added when d came.
	if r.len() != 3 || !r.contains("a") || r.contains("b") || !r.contains("c") || !r.contains("d") {
		t.Fatalf("len %d a=%t b=%t c=%t d=%t", r.len(), r.contains("a"), r.contains("b"), r.contains("c"), r.contains("d"))
	}
	r.remove("a")
	r.remove("zzz")
	if r.len() != 2 || r.contains("a") || len(r.index) != 2 {
		t.Fatalf("after remove: %d", r.len())
	}
	for i := range 1000 {
		r.add(fmt.Sprint(i))
	}
	if r.len() != 3 || len(r.index) != 3 {
		t.Fatalf("unbounded: list %d index %d", r.len(), len(r.index))
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
