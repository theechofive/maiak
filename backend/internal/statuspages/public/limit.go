package public

import (
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

// Limiter is a token bucket per key, in memory and per replica (L-274). Memory is bounded: when
// the table is full, the least recently used key is dropped (one at a time, so a flood of new keys
// only forgets the oldest ones, never every key at once).
type Limiter struct {
	mu      sync.Mutex
	clock   clock.Clock
	rate    float64 // tokens per second
	burst   float64
	maxKeys int
	buckets map[string]*list.Element
	lru     *list.List // front = most recently used
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

// NewLimiter allows burst requests at once and rate per second on average for each key.
func NewLimiter(rate float64, burst, maxKeys int, c clock.Clock) *Limiter {
	if c == nil {
		c = clock.System{}
	}
	return &Limiter{clock: c, rate: rate, burst: float64(burst), maxKeys: maxKeys, buckets: map[string]*list.Element{}, lru: list.New()}
}

// refill returns the key's bucket, refilled to now and marked as recently used.
func (l *Limiter) refill(key string, now time.Time) *bucket {
	if e, ok := l.buckets[key]; ok {
		l.lru.MoveToFront(e)
		b := e.Value.(*bucket)
		b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
		b.last = now
		return b
	}
	for len(l.buckets) >= l.maxKeys {
		oldest := l.lru.Back()
		delete(l.buckets, oldest.Value.(*bucket).key)
		l.lru.Remove(oldest)
	}
	b := &bucket{key: key, tokens: l.burst, last: now}
	l.buckets[key] = l.lru.PushFront(b)
	return b
}

func (l *Limiter) wait(b *bucket) time.Duration {
	return max(time.Duration((1-b.tokens)/l.rate*float64(time.Second)), time.Second)
}

// Allow takes a token for key; when there is none it returns false and the wait until the next one.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key, l.clock.Now())
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, l.wait(b)
}

// Has reports whether key has a token, without taking it.
func (l *Limiter) Has(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key, l.clock.Now())
	if b.tokens >= 1 {
		return true, 0
	}
	return false, l.wait(b)
}

// Take spends a token of key even when there is none left (it then goes negative, up to one burst),
// for counting an outcome after the fact, such as a wrong password.
func (l *Limiter) Take(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key, l.clock.Now())
	b.tokens = math.Max(b.tokens-1, -l.burst)
}

// Len is the number of keys tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Cookie of an unlocked password-protected page (ADR-0103). Its value is the expiry and an
// HMAC-SHA256 over the version, the slug, a digest of the password hash and the expiry, keyed with
// MAIAK_STATUSPAGE_COOKIE_KEY: it unlocks one page until it expires or the password changes.
const (
	CookieName = "maiak_status_unlock"
	// CookieTTL is how long an unlocked page stays unlocked in a browser.
	CookieTTL = 12 * time.Hour
)

// CookieSigner signs and checks unlock cookies.
type CookieSigner struct {
	key []byte
}

// NewCookieSigner returns a signer; key must be at least 32 bytes.
func NewCookieSigner(key []byte) *CookieSigner {
	return &CookieSigner{key: append([]byte(nil), key...)}
}

func (s *CookieSigner) mac(slug, passwordHash string, expires int64) []byte {
	ph := sha256.Sum256([]byte(passwordHash))
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("maiak-statuspage-unlock-v1\x00"))
	m.Write([]byte(slug))
	m.Write([]byte{0})
	m.Write(ph[:])
	var e [8]byte
	binary.BigEndian.PutUint64(e[:], uint64(expires)) //nolint:gosec // G115: unix seconds are positive
	m.Write(e[:])
	return m.Sum(nil)
}

// Value returns the cookie value for a page unlocked now.
func (s *CookieSigner) Value(slug, passwordHash string, now time.Time) string {
	exp := now.Add(CookieTTL).Unix()
	buf := make([]byte, 8, 8+sha256.Size)
	binary.BigEndian.PutUint64(buf, uint64(exp)) //nolint:gosec // G115: unix seconds are positive
	buf = append(buf, s.mac(slug, passwordHash, exp)...)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// Valid reports whether value unlocks the page with this password hash at now.
func (s *CookieSigner) Valid(value, slug, passwordHash string, now time.Time) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 8+sha256.Size {
		return false
	}
	exp := int64(binary.BigEndian.Uint64(raw[:8])) //nolint:gosec // G115: compared below, an overflow is just invalid
	if exp <= now.Unix() || exp > now.Add(CookieTTL+time.Minute).Unix() {
		return false
	}
	return hmac.Equal(raw[8:], s.mac(slug, passwordHash, exp))
}
