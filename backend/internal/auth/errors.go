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
	// ErrAccountSuspended means the login and password are right but the user's tenant is
	// suspended or deleted by the platform admin. It is only returned after a correct password.
	ErrAccountSuspended = errors.New("auth: account suspended")
	// ErrUnauthenticated means no valid session.
	ErrUnauthenticated = errors.New("auth: not authenticated")
	// ErrCSRF means the CSRF token is missing or wrong.
	ErrCSRF = errors.New("auth: invalid csrf token")
	// ErrRateLimited means too many failed attempts; see RateLimitedError for the delay.
	ErrRateLimited = errors.New("auth: too many attempts")
	// ErrAdminLoginInUse stops BootstrapAdmin when the configured login belongs to a user with a
	// membership outside the default tenant, for example someone who registered that email.
	ErrAdminLoginInUse = errors.New("auth: MAIAK_ADMIN_LOGIN belongs to a user of another tenant; " +
		"refusing to make it the admin (choose another login or remove that user's other memberships)")
)

// RateLimitedError carries the time until the next attempt may be accepted.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitedError) Is(target error) bool { return target == ErrRateLimited }
