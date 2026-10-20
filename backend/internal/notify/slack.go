// Package notify owns notification channels (notification_channels; monitor_channels is unused
// in the MVP, ADR-0019) and sends Slack messages. The webhook URL is a secret: it is stored
// encrypted (AES-256-GCM, platform/crypto), never returned by the api, never logged, and errors
// from sending it are reduced to kinds, because raw errors of the guarded client carry resolved
// addresses and the URL itself (ADR-0016, ADR-0018).
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/theechofive/maiak/backend/internal/safehttp"
)

// SlackHost is the only host a webhook may point at.
const SlackHost = "hooks.slack.com"

// MaxWebhookURLLength caps the stored URL (real webhook URLs are about 80 characters).
const MaxWebhookURLLength = 300

// SendTimeout bounds one Slack request.
const SendTimeout = 10 * time.Second

// maxRetryAfter caps a Retry-After header so a hostile or broken answer cannot park a delivery
// for days.
const maxRetryAfter = time.Hour

var webhookPattern = regexp.MustCompile(`^https://hooks\.slack\.com/services/[A-Za-z0-9_/\-]+$`)

// ValidateWebhookURL returns a user-facing message when raw is not an acceptable Slack Incoming
// Webhook URL, or "" when it is. The message never repeats the value.
func ValidateWebhookURL(raw string) string {
	if raw == "" {
		return "is required"
	}
	if len(raw) > MaxWebhookURLLength {
		return "must be at most " + strconv.Itoa(MaxWebhookURLLength) + " characters"
	}
	if !webhookPattern.MatchString(raw) {
		return "must be a Slack Incoming Webhook URL (https://hooks.slack.com/services/...)"
	}
	// The pattern already pins scheme, host and path; parsing again guards against a future
	// edit of the pattern letting a port, user info or another host through.
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != SlackHost || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "must be a Slack Incoming Webhook URL (https://hooks.slack.com/services/...)"
	}
	return ""
}

// Error kinds of a failed send. They are the only failure detail stored or logged.
const (
	KindInvalidURL     = "invalid_url"
	KindNotFound       = "webhook_not_found" // 404 or 410: the webhook was revoked
	KindRateLimited    = "rate_limited"
	KindHTTPStatus     = "http_status"
	KindTimeout        = "timeout"
	KindDNS            = "dns"
	KindConnect        = "connect"
	KindTLS            = "tls"
	KindBlockedAddress = "blocked_address"
	KindOther          = "other"
)

// SendError is a failed send. It never wraps the underlying error.
type SendError struct {
	Kind string
	// Status is the HTTP status Slack answered with, 0 when no response arrived.
	Status int
	// Permanent means retrying cannot help (revoked webhook, invalid URL).
	Permanent bool
	// RetryAfter is Slack's Retry-After on 429, 0 when absent.
	RetryAfter time.Duration
}

func (e *SendError) Error() string {
	if e.Status != 0 {
		return "notify: slack send failed: " + e.Kind + " (HTTP " + strconv.Itoa(e.Status) + ")"
	}
	return "notify: slack send failed: " + e.Kind
}

// Doer sends an HTTP request. In production it is the guarded client from NewSlackClient;
// tests use a plain client against httptest servers (the guard refuses loopback, L-041).
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewSlackClient returns the SSRF-guarded client for Slack: no redirects, no proxy,
// connect-time address validation with the operator's extra blocked ranges.
func NewSlackClient(blocked []netip.Prefix) *http.Client {
	return safehttp.NewClient(safehttp.Options{Timeout: SendTimeout, MaxRedirects: 0, BlockedPrefixes: blocked})
}

// SlackNotifier posts messages to Slack Incoming Webhooks.
type SlackNotifier struct {
	client Doer
	now    func() time.Time
}

// NewSlackNotifier returns a notifier sending through client.
func NewSlackNotifier(client Doer) *SlackNotifier {
	return &SlackNotifier{client: client, now: time.Now}
}

// Send posts text (Slack mrkdwn) to webhookURL. The URL is validated again before every send.
// Every failure is a *SendError.
func (n *SlackNotifier) Send(ctx context.Context, webhookURL, text string) error {
	if ValidateWebhookURL(webhookURL) != "" {
		return &SendError{Kind: KindInvalidURL, Permanent: true}
	}
	body, err := json.Marshal(struct {
		Text string `json:"text"`
	}{text})
	if err != nil {
		return &SendError{Kind: KindOther}
	}
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return &SendError{Kind: KindInvalidURL, Permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Maiak/1 (+notifications)")

	resp, err := n.client.Do(req)
	if err != nil {
		return &SendError{Kind: classify(ctx, err)}
	}
	// Slack's answer is never shown or stored; read a little so the connection closes cleanly.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return &SendError{Kind: KindNotFound, Status: resp.StatusCode, Permanent: true}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &SendError{Kind: KindRateLimited, Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), n.now())}
	}
	return &SendError{Kind: KindHTTPStatus, Status: resp.StatusCode}
}

// parseRetryAfter reads delay-seconds or an HTTP date; anything else is 0 (use the backoff).
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		d = time.Duration(min(secs, int(maxRetryAfter/time.Second))) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, 0), maxRetryAfter)
}

func classify(ctx context.Context, err error) string {
	var (
		dnsErr    *net.DNSError
		netErr    net.Error
		opErr     *net.OpError
		recordErr tls.RecordHeaderError
		alertErr  tls.AlertError
		verifyErr *tls.CertificateVerificationError
		authErr   x509.UnknownAuthorityError
		hostErr   x509.HostnameError
	)
	switch {
	case errors.Is(err, safehttp.ErrBlockedAddress):
		return KindBlockedAddress
	case errors.Is(err, safehttp.ErrBadPort), errors.Is(err, safehttp.ErrNotHTTPS),
		errors.Is(err, safehttp.ErrTooManyRedirects), errors.Is(err, safehttp.ErrRedirectNotHTTPS):
		return KindOther
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded),
		errors.As(err, &netErr) && netErr.Timeout():
		return KindTimeout
	case errors.As(err, &dnsErr):
		return KindDNS
	case errors.As(err, &verifyErr), errors.As(err, &authErr), errors.As(err, &hostErr),
		errors.As(err, &recordErr), errors.As(err, &alertErr):
		return KindTLS
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return KindConnect
	}
	return KindOther
}
