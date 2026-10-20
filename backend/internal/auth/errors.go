package auth

import (
	"errors"
	"time"
)

// Domain errors. They are mapped to HTTP problems in one place (internal/api).
var (
	// ErrInvalidCredentials is returned for every login failure, whatever the cause, so the
	// response does not reveal whether the login exists.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrUnauthenticated means no valid session.
	ErrUnauthenticated = errors.New("auth: not authenticated")
	// ErrCSRF means the CSRF token is missing or wrong.
	ErrCSRF = errors.New("auth: invalid csrf token")
	// ErrRateLimited means too many failed attempts; see RateLimitedError for the delay.
	ErrRateLimited = errors.New("auth: too many attempts")
)

// RateLimitedError carries the time until the next attempt may be accepted.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitedError) Is(target error) bool { return target == ErrRateLimited }
