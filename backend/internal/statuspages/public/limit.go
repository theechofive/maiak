package public

import (
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
// the table is full, idle buckets are dropped first, then arbitrary ones.
type Limiter struct {
	mu      sync.Mutex
	clock   clock.Clock
	rate    float64 // tokens per second
	burst   float64
	maxKeys int
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter allows burst requests at once and rate per second on average for each key.
func NewLimiter(rate float64, burst, maxKeys int, c clock.Clock) *Limiter {
	if c == nil {
		c = clock.System{}
	}
	return &Limiter{clock: c, rate: rate, burst: float64(burst), maxKeys: maxKeys, buckets: map[string]*bucket{}}
}

// Allow takes a token for key; when there is none it returns false and the wait until the next one.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.evict(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, max(wait, time.Second)
}

func (l *Limiter) evict(now time.Time) {
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
		}
	}
	for k := range l.buckets {
		if len(l.buckets) < l.maxKeys/2 {
			return
		}
		delete(l.buckets, k)
	}
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
