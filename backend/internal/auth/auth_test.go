package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

func TestLimiter(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	l := NewLimiter(3, time.Minute, 100, c)

	for i := 0; i < 3; i++ {
		if ok, _ := l.Reserve("ip"); !ok {
			t.Fatalf("attempt %d blocked too early", i)
		}
	}
	ok, wait := l.Reserve("ip")
	if ok || wait != time.Minute {
		t.Fatalf("after 3 attempts: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Reserve("other"); !ok {
		t.Fatal("keys must be independent")
	}

	c.Advance(40 * time.Second)
	if ok, wait := l.Reserve("ip"); ok || wait != 20*time.Second {
		t.Fatalf("mid-window: ok=%v wait=%v", ok, wait)
	}
	// A refunded attempt (a success) frees one slot.
	l.Refund("ip")
	if ok, _ := l.Reserve("ip"); !ok {
		t.Fatal("refunded attempt should be available again")
	}
	if ok, _ := l.Reserve("ip"); ok {
		t.Fatal("only one attempt was refunded")
	}
	c.Advance(20 * time.Second)
	if ok, _ := l.Reserve("ip"); !ok {
		t.Fatal("window expired, attempt should be allowed")
	}

	for i := 0; i < 3; i++ {
		l.Reserve("mail")
	}
	l.Reset("mail")
	if ok, _ := l.Reserve("mail"); !ok {
		t.Fatal("reset key should be allowed")
	}
}

// Reservation is atomic: however many requests race, at most limit of them get through.
func TestLimiterReserveIsAtomic(t *testing.T) {
	l := NewLimiter(10, time.Hour, 100, clock.NewFake(time.Now()))
	var wg sync.WaitGroup
	var allowed atomic.Int32
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Reserve("admin@example.com"); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := allowed.Load(); n != 10 {
		t.Fatalf("%d attempts reserved, limit is 10", n)
	}
}

func TestLimiterBoundsMemory(t *testing.T) {
	c := clock.NewFake(time.Now())
	l := NewLimiter(1, time.Hour, 10, c)
	for i := 0; i < 1000; i++ {
		l.Reserve(fmt.Sprintf("k%d", i))
	}
	if n := len(l.entries); n > 10 {
		t.Fatalf("limiter tracks %d keys, cap is 10", n)
	}
	// The most recent key is always tracked.
	if ok, _ := l.Reserve("k999"); ok {
		t.Fatal("latest key should still be limited")
	}
}

func TestCSRFValid(t *testing.T) {
	cases := []struct {
		expected, got string
		want          bool
	}{
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"abc", "ab", false},
		{"abc", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := CSRFValid(c.expected, c.got); got != c.want {
			t.Errorf("CSRFValid(%q, %q) = %v", c.expected, c.got, got)
		}
	}
}

func TestTokens(t *testing.T) {
	a, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newToken()
	if a == b || !validTokenFormat(a) {
		t.Fatalf("tokens: %q %q", a, b)
	}
	if len(hashToken(a)) != 32 || string(hashToken(a)) == string(hashToken(b)) {
		t.Fatal("hashToken must be a 32-byte digest")
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 44), "!!!" + a[3:]} {
		if validTokenFormat(bad) {
			t.Errorf("validTokenFormat(%q) = true", bad)
		}
	}
}

func TestRateLimitedError(t *testing.T) {
	var err error = &RateLimitedError{RetryAfter: time.Second}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatal("RateLimitedError must match ErrRateLimited")
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", err), ErrRateLimited) {
		t.Fatal("wrapped RateLimitedError must match ErrRateLimited")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatal(got)
	}
	got := truncate(strings.Repeat("ж", 10), 5) // 2 bytes per rune
	if len(got) > 5 || !utf8.ValidString(got) {
		t.Fatalf("truncate cut inside a rune: %q", got)
	}
	if got := truncate("a\xffb", 10); got != "ab" {
		t.Fatalf("invalid UTF-8 not removed: %q", got)
	}
}

func TestAcquireHashWaitIsBounded(t *testing.T) {
	s, err := NewService(nil, Options{HashQueueWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxConcurrentHashes; i++ {
		if err := s.acquireHash(context.Background()); err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
	}
	start := time.Now()
	err = s.acquireHash(context.Background())
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Fatalf("full queue: err = %v, want RateLimitedError", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	<-s.hashSem
	if err := s.acquireHash(context.Background()); err != nil {
		t.Fatalf("freed slot: %v", err)
	}
}
