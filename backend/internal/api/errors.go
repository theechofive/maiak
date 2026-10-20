package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"

	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

// Problem types (RFC 9457 "type" members).
const (
	problemBadRequest         = "urn:maiak:problem:bad-request"
	problemUnauthenticated    = "urn:maiak:problem:unauthenticated"
	problemInvalidCredentials = "urn:maiak:problem:invalid-credentials"
	problemCSRF               = "urn:maiak:problem:csrf"
	problemRateLimited        = "urn:maiak:problem:rate-limited"
	problemNotFound           = "urn:maiak:problem:not-found"
	problemMethodNotAllowed   = "urn:maiak:problem:method-not-allowed"
	problemInternal           = "urn:maiak:problem:internal"
)

// badRequestError is a request the handler itself rejected (the validator catches most).
type badRequestError struct{ detail string }

func (e *badRequestError) Error() string { return "bad request: " + e.detail }

var (
	errNotFound         = errors.New("not found")
	errMethodNotAllowed = errors.New("method not allowed")
)

// writeError is the single place where errors become HTTP responses. Unknown errors are
// logged and returned as a generic 500 without detail.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	p := httpx.Problem{RequestID: logging.RequestID(r.Context()), Instance: r.URL.Path}

	var badReq *badRequestError
	var reqErr *openapi3filter.RequestError
	var rateErr *auth.RateLimitedError
	switch {
	case errors.As(err, &badReq):
		p.Type, p.Title, p.Status, p.Detail = problemBadRequest, "Bad request", http.StatusBadRequest, badReq.detail
	case errors.As(err, &reqErr):
		p.Type, p.Title, p.Status, p.Detail = problemBadRequest, "Bad request", http.StatusBadRequest, requestErrorDetail(reqErr)
	case errors.Is(err, auth.ErrCSRF):
		p.Type, p.Title, p.Status = problemCSRF, "Missing or invalid CSRF token", http.StatusForbidden
	case errors.Is(err, auth.ErrUnauthenticated):
		p.Type, p.Title, p.Status = problemUnauthenticated, "Not authenticated", http.StatusUnauthorized
	case errors.Is(err, auth.ErrInvalidCredentials):
		p.Type, p.Title, p.Status = problemInvalidCredentials, "Invalid email or password", http.StatusUnauthorized
	case errors.As(err, &rateErr):
		secs := int(math.Ceil(rateErr.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		p.Type, p.Title, p.Status = problemRateLimited, "Too many attempts, try again later", http.StatusTooManyRequests
	case errors.Is(err, errNotFound), errors.Is(err, routers.ErrPathNotFound):
		p.Type, p.Title, p.Status = problemNotFound, "Not found", http.StatusNotFound
	case errors.Is(err, errMethodNotAllowed), errors.Is(err, routers.ErrMethodNotAllowed):
		p.Type, p.Title, p.Status = problemMethodNotAllowed, "Method not allowed", http.StatusMethodNotAllowed
	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads the response.
		log.InfoContext(r.Context(), "request cancelled", "error", err)
		p.Type, p.Title, p.Status = problemInternal, "Request cancelled", 499
	default:
		log.ErrorContext(r.Context(), "request failed", "error", err)
		p.Type, p.Title, p.Status = problemInternal, "Internal server error", http.StatusInternalServerError
	}
	httpx.WriteProblem(w, p)
}

// requestErrorDetail describes a validation failure without echoing the submitted value
// (it may be a password).
func requestErrorDetail(e *openapi3filter.RequestError) string {
	var where string
	switch {
	case e.Parameter != nil:
		where = fmt.Sprintf("%s parameter %q", e.Parameter.In, e.Parameter.Name)
	case e.RequestBody != nil:
		where = "request body"
	default:
		where = "request"
	}
	var se *openapi3.SchemaError
	if errors.As(e.Err, &se) {
		if ptr := se.JSONPointer(); len(ptr) > 0 {
			return fmt.Sprintf("%s: field %q: %s", where, strings.Join(ptr, "."), se.Reason)
		}
		return fmt.Sprintf("%s: %s", where, se.Reason)
	}
	if e.Reason != "" {
		return fmt.Sprintf("%s: %s", where, e.Reason)
	}
	return where + " is invalid"
}
