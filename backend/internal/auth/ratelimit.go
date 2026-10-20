package auth

import (
	"sync"
	"time"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

// Limiter counts attempts per key in a fixed window that starts at the first attempt.
// It is in memory, so each api replica limits independently (ledger: per-replica limit).
//
// An attempt is reserved (counted) before the expensive check runs, under the same lock as
// the limit test, so concurrent requests cannot all pass the test before any of them is
// counted. A successful attempt is refunded.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	clock   clock.Clock
	entries map[string]*limitEntry
}

type limitEntry struct {
	start    time.Time
	attempts int
}

// NewLimiter allows up to limit attempts per key per window and tracks at most maxKeys keys.
func NewLimiter(limit int, window time.Duration, maxKeys int, c clock.Clock) *Limiter {
	return &Limiter{limit: limit, window: window, maxKeys: maxKeys, clock: c, entries: map[string]*limitEntry{}}
}

// Reserve counts one attempt for key if the limit allows it. Otherwise it reports false and
// how long until the key may attempt again.
func (l *Limiter) Reserve(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	e, ok := l.entries[key]
	if ok && now.Sub(e.start) >= l.window {
		ok = false
	}
	if !ok {
		if len(l.entries) >= l.maxKeys {
			l.evictLocked(now)
		}
		e = &limitEntry{start: now}
		l.entries[key] = e
	}
	if e.attempts >= l.limit {
		return false, e.start.Add(l.window).Sub(now)
	}
	e.attempts++
	return true, 0
}

// Refund gives back one reserved attempt (the attempt succeeded or never ran).
func (l *Limiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok && e.attempts > 0 {
		e.attempts--
	}
}

// Reset forgets key (after a successful login).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// evictLocked drops expired entries; if the table is still full it drops arbitrary entries,
// trading accuracy for bounded memory under a flood of distinct keys.
func (l *Limiter) evictLocked(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.start) >= l.window {
			delete(l.entries, k)
		}
	}
	for k := range l.entries {
		if len(l.entries) < l.maxKeys {
			return
		}
		delete(l.entries, k)
	}
}
