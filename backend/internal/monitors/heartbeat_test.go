package monitors

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestHeartbeatTokenAndHash(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		token, hash, err := NewHeartbeatToken()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(raw) != 32 || len(token) != 43 {
			t.Fatalf("token %q: %d bytes, err %v; want 32 bytes of URL-safe base64 without padding", token, len(raw), err)
		}
		sum := sha256.Sum256([]byte(token))
		if !bytes.Equal(hash, sum[:]) || !bytes.Equal(HashHeartbeatToken(token), hash) {
			t.Fatal("hash is not the SHA-256 of the token as written in the URL")
		}
		if seen[token] {
			t.Fatal("token repeated")
		}
		seen[token] = true
	}
	// Any string hashes to 32 bytes, so a malformed token is looked up like any other.
	for _, s := range []string{"", "x", "../../etc/passwd", string(make([]byte, 4096))} {
		if len(HashHeartbeatToken(s)) != 32 {
			t.Fatalf("hash of %q is not 32 bytes", s)
		}
	}
}

func TestHeartbeatValidationBounds(t *testing.T) {
	for n, ok := range map[int]bool{59: false, 60: true, 300: true, 86400: true, 86401: false, -1: false} {
		if got := ValidateExpectedInterval(n) == ""; got != ok {
			t.Errorf("expected interval %d: valid=%t, want %t", n, got, ok)
		}
	}
	for n, ok := range map[int]bool{-1: false, 0: true, 60: true, 3600: true, 3601: false} {
		if got := ValidateGrace(n) == ""; got != ok {
			t.Errorf("grace %d: valid=%t, want %t", n, got, ok)
		}
	}
	if ValidateExpectedInterval(DefaultExpectedIntervalSeconds) != "" || ValidateGrace(DefaultGraceSeconds) != "" {
		t.Fatal("defaults must be valid")
	}
}

func TestNextHeartbeatStep(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	base := heartbeatState{WaitingSince: t0, Interval: 5 * time.Minute, Grace: time.Minute}
	with := func(f func(*heartbeatState)) heartbeatState { s := base; f(&s); return s }
	cases := []struct {
		name       string
		st         heartbeatState
		now        time.Duration
		ping, miss *time.Time
	}{
		{"never pinged, before the deadline", base, 5 * time.Minute, nil, nil},
		{"never pinged, exactly at the deadline", base, 6 * time.Minute, nil, nil},
		{"never pinged, deadline passed", base, 6*time.Minute + time.Second, nil, at(6 * time.Minute)},
		{"pinged, ping not applied yet", with(func(s *heartbeatState) { s.LastPing = at(2 * time.Minute) }), 3 * time.Minute, at(2 * time.Minute), nil},
		{"pinged and applied", with(func(s *heartbeatState) { s.LastPing, s.AppliedPing = at(2*time.Minute), at(2*time.Minute) }), 3 * time.Minute, nil, nil},
		{"deadline counts from the last ping",
			with(func(s *heartbeatState) { s.LastPing, s.AppliedPing = at(2*time.Minute), at(2*time.Minute) }), 7 * time.Minute, nil, nil},
		{"missed after the last ping",
			with(func(s *heartbeatState) { s.LastPing, s.AppliedPing = at(2*time.Minute), at(2*time.Minute) }), 9 * time.Minute, nil, at(8 * time.Minute)},
		{"missed deadline applied once", with(func(s *heartbeatState) { s.MissedDeadline = at(6 * time.Minute) }), time.Hour, nil, nil},
		{"new ping after a miss resolves, the next deadline is far",
			with(func(s *heartbeatState) { s.MissedDeadline, s.LastPing = at(6*time.Minute), at(30*time.Minute) }), 31 * time.Minute, at(30 * time.Minute), nil},
		{"paused: nothing", with(func(s *heartbeatState) { s.Paused, s.LastPing = true, at(time.Minute) }), time.Hour, nil, nil},
		{"resume restarts the wait: an old ping does not set the deadline",
			with(func(s *heartbeatState) {
				s.WaitingSince, s.LastPing, s.AppliedPing = t0.Add(time.Hour), at(time.Minute), at(time.Minute)
			}), time.Hour + 5*time.Minute, nil, nil},
		{"resume, deadline passed", with(func(s *heartbeatState) {
			s.WaitingSince, s.LastPing, s.AppliedPing = t0.Add(time.Hour), at(time.Minute), at(time.Minute)
		}), time.Hour + 7*time.Minute, nil, at(time.Hour + 6*time.Minute)},
		{"grace 0", with(func(s *heartbeatState) { s.Grace = 0 }), 5*time.Minute + time.Millisecond, nil, at(5 * time.Minute)},
		{"ping and miss in one step: the miss counts from the ping",
			with(func(s *heartbeatState) { s.LastPing = at(time.Minute) }), 8 * time.Minute, at(time.Minute), at(7 * time.Minute)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextHeartbeatStep(c.st, t0.Add(c.now))
			if !sameTime(got.Ping, c.ping) || !sameTime(got.Missed, c.miss) {
				t.Fatalf("step = ping %v missed %v; want ping %v missed %v", got.Ping, got.Missed, c.ping, c.miss)
			}
		})
	}
}

func TestHeartbeatDeadline(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	early, late := t0.Add(-time.Hour), t0.Add(time.Minute)
	if got := HeartbeatDeadline(t0, nil, time.Minute, 0); !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("never pinged: %v", got)
	}
	if got := HeartbeatDeadline(t0, &early, time.Minute, time.Second); !got.Equal(t0.Add(time.Minute + time.Second)) {
		t.Fatalf("ping before the wait started: %v", got)
	}
	if got := HeartbeatDeadline(t0, &late, time.Minute, time.Second); !got.Equal(late.Add(time.Minute + time.Second)) {
		t.Fatalf("pinged: %v", got)
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
