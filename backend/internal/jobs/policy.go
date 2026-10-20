package jobs

import (
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/notify"
)

// MaxAttempts is how many sends a delivery gets before it is marked failed (ADR-0016).
const MaxAttempts = 5

// retryDelays is the backoff after the n-th failed attempt (index n-1); the last value repeats.
var retryDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Backoff returns the delay before the next attempt after attempts failed attempts (>= 1).
func Backoff(attempts int) time.Duration {
	return retryDelays[min(max(attempts, 1), len(retryDelays))-1]
}

// Delivery statuses passed to finish_delivery.
const (
	statusSent    = "sent"
	statusFailed  = "failed"
	statusPending = "pending"
)

// outcome is what to record for one attempt.
type outcome struct {
	status string
	// errKind is stored in last_error; never a raw error text.
	errKind string
	retryAt time.Time
	// failChannel marks the channel failed: the webhook was revoked or the stored URL is invalid,
	// so every later delivery to it would fail the same way.
	failChannel bool
}

// decide maps a send result to what finish_delivery records. attempts is the number of earlier
// attempts (the claimed row's value); this attempt is attempts+1.
func decide(attempts int, sendErr error, now time.Time) outcome {
	if sendErr == nil {
		return outcome{status: statusSent}
	}
	var se *notify.SendError
	if !errors.As(sendErr, &se) {
		se = &notify.SendError{Kind: notify.KindOther}
	}
	kind := se.Kind
	if se.Status != 0 && se.Kind == notify.KindHTTPStatus {
		kind = se.Kind + " " + strconv.Itoa(se.Status)
	}
	if se.Permanent {
		return outcome{status: statusFailed, errKind: kind, failChannel: se.Kind == notify.KindNotFound || se.Kind == notify.KindInvalidURL}
	}
	done := attempts + 1
	if done >= MaxAttempts {
		return outcome{status: statusFailed, errKind: kind}
	}
	return outcome{status: statusPending, errKind: kind, retryAt: now.Add(max(Backoff(done), se.RetryAfter))}
}

// channelLimiter spaces messages to one channel at least interval apart (per jobs process).
// reserve books the next free slot and returns how long the caller must wait for it.
type channelLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     map[uuid.UUID]time.Time
}

func newChannelLimiter(interval time.Duration) *channelLimiter {
	return &channelLimiter{interval: interval, next: map[uuid.UUID]time.Time{}}
}

func (l *channelLimiter) reserve(channel uuid.UUID, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot := l.next[channel]
	if slot.Before(now) {
		slot = now
	}
	l.next[channel] = slot.Add(l.interval)
	if len(l.next) > 1024 {
		for ch, t := range l.next {
			if !t.After(now) {
				delete(l.next, ch)
			}
		}
	}
	return slot.Sub(now)
}
