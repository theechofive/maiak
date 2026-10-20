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
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/scheduler"
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
	problemValidation         = "urn:maiak:problem:validation"
	problemNotImplemented     = "urn:maiak:problem:not-implemented"
	problemLimitReached       = "urn:maiak:problem:limit-reached"
	problemSlackNotConfigured = "urn:maiak:problem:slack-not-configured"
	problemSlackFailed        = "urn:maiak:problem:slack-delivery-failed"
	problemUnavailable        = "urn:maiak:problem:unavailable"
	problemInternal           = "urn:maiak:problem:internal"
)

// badRequestError is a request the handler itself rejected (the validator catches most).
type badRequestError struct{ detail string }

func (e *badRequestError) Error() string { return "bad request: " + e.detail }

var (
	errNotFound         = errors.New("not found")
	errMethodNotAllowed = errors.New("method not allowed")
	errNotImplemented   = errors.New("not implemented")
)

// writeError is the single place where errors become HTTP responses. Unknown errors are
// logged and returned as a generic 500 without detail.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	p := httpx.Problem{RequestID: logging.RequestID(r.Context()), Instance: r.URL.Path}

	var badReq *badRequestError
	var reqErr *openapi3filter.RequestError
	var rateErr *auth.RateLimitedError
	var valErr *validation.Error
	var limitErr *monitors.LimitError
	var sendErr *notify.SendError
	switch {
	case errors.As(err, &badReq):
		p.Type, p.Title, p.Status, p.Detail = problemBadRequest, "Bad request", http.StatusBadRequest, badReq.detail
	case errors.As(err, &reqErr):
		p.Type, p.Title, p.Status, p.Detail = problemBadRequest, "Bad request", http.StatusBadRequest, requestErrorDetail(reqErr)
		p.FieldErrors = requestFieldErrors(reqErr)
	case errors.As(err, &valErr):
		p.Type, p.Title, p.Status = problemValidation, "Validation failed", http.StatusUnprocessableEntity
		for _, f := range valErr.Fields {
			p.FieldErrors = append(p.FieldErrors, httpx.FieldError{Field: f.Field, Message: f.Message})
		}
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
	case errors.As(err, &limitErr):
		p.Type, p.Title, p.Status = problemLimitReached, "Monitor limit reached", http.StatusConflict
		p.Detail = fmt.Sprintf("This tenant can have at most %d monitors.", limitErr.Limit)
	case errors.Is(err, notify.ErrNotConfigured):
		p.Type, p.Title, p.Status = problemSlackNotConfigured, "Slack is not configured", http.StatusConflict
	case errors.As(err, &sendErr) && sendErr.Kind == notify.KindRateLimited:
		secs := int(math.Ceil(sendErr.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		p.Type, p.Title, p.Status = problemRateLimited, "Slack is rate-limiting this webhook, try again later", http.StatusTooManyRequests
	case errors.As(err, &sendErr):
		// Only the kind is shown: Slack's answer and transport errors are never echoed.
		p.Type, p.Title, p.Status, p.Detail = problemSlackFailed, "Slack did not accept the message", http.StatusBadGateway, slackFailureDetail(sendErr)
	case errors.Is(err, scheduler.ErrNoLocation):
		// Operational, not the caller's fault: the location that runs new monitors is missing or
		// disabled (ADR-0018).
		log.ErrorContext(r.Context(), "request failed", "error", err)
		p.Type, p.Title, p.Status = problemUnavailable, "Monitoring location unavailable, try again later", http.StatusServiceUnavailable
	case errors.Is(err, errNotFound), errors.Is(err, routers.ErrPathNotFound), errors.Is(err, monitors.ErrNotFound), errors.Is(err, queries.ErrNotFound):
		p.Type, p.Title, p.Status = problemNotFound, "Not found", http.StatusNotFound
	case errors.Is(err, errMethodNotAllowed), errors.Is(err, routers.ErrMethodNotAllowed):
		p.Type, p.Title, p.Status = problemMethodNotAllowed, "Method not allowed", http.StatusMethodNotAllowed
	case errors.Is(err, errNotImplemented):
		p.Type, p.Title, p.Status = problemNotImplemented, "Not implemented yet", http.StatusNotImplemented
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

// requestFieldErrors attributes a request body schema violation to its field, so clients can
// show it next to the input. Parameters and unparsable bodies have no field.
func requestFieldErrors(e *openapi3filter.RequestError) []httpx.FieldError {
	var se *openapi3.SchemaError
	if e.RequestBody == nil || !errors.As(e.Err, &se) {
		return nil
	}
	ptr := se.JSONPointer()
	if len(ptr) == 0 {
		return nil
	}
	return []httpx.FieldError{{Field: strings.Join(ptr, "."), Message: se.Reason}}
}

func slackFailureDetail(e *notify.SendError) string {
	switch e.Kind {
	case notify.KindNotFound:
		return "Slack does not know this webhook any more (it was removed or revoked); store a new webhook URL."
	case notify.KindHTTPStatus:
		return fmt.Sprintf("Slack answered HTTP %d.", e.Status)
	case notify.KindTimeout:
		return "Slack did not answer in time."
	}
	return "Slack could not be reached (" + e.Kind + ")."
}
