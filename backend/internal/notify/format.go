package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Kinds of outbox deliveries (notification_deliveries.kind).
const (
	DeliveryOpened      = "opened"
	DeliveryResolved    = "resolved"
	DeliveryTLSExpiring = "tls_expiring"
)

// Payload is the outbox payload written by internal/incidents (ADR-0019). It holds what a
// message needs and no secret.
type Payload struct {
	MonitorID       uuid.UUID  `json:"monitorId"`
	MonitorName     string     `json:"monitorName"`
	URL             string     `json:"url"`
	IncidentID      *uuid.UUID `json:"incidentId,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	ResolvedAt      *time.Time `json:"resolvedAt,omitempty"`
	DurationSeconds int64      `json:"durationSeconds,omitempty"`
	TLSNotAfter     *time.Time `json:"tlsNotAfter,omitempty"`
	DaysLeft        *int       `json:"daysLeft,omitempty"`
}

// ErrBadPayload means an outbox row cannot be turned into a message; retrying will not help.
var ErrBadPayload = errors.New("notify: unusable delivery payload")

// Format renders the Slack mrkdwn text of an outbox delivery (formats fixed by ADR-0016).
// publicURL is MAIAK_PUBLIC_URL without a trailing slash.
func Format(kind string, raw []byte, publicURL string) (string, error) {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", ErrBadPayload
	}
	switch kind {
	case DeliveryOpened:
		return FormatOpened(p, publicURL), nil
	case DeliveryResolved:
		return FormatResolved(p), nil
	case DeliveryTLSExpiring:
		if p.TLSNotAfter == nil || p.DaysLeft == nil {
			return "", ErrBadPayload
		}
		return FormatTLSExpiring(p), nil
	}
	return "", ErrBadPayload
}

// FormatOpened: the headline, then the URL, the reason, the start time in UTC and a link.
func FormatOpened(p Payload, publicURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, ":red_circle: *%s* is down", Escape(p.MonitorName))
	if u := DisplayURL(p.URL); u != "" {
		b.WriteString("\n" + Escape(u))
	}
	reason := p.Reason
	if reason == "" {
		reason = "unknown"
	}
	b.WriteString("\nReason: " + Escape(reason))
	if p.StartedAt != nil {
		b.WriteString("\nSince: " + p.StartedAt.UTC().Format("2006-01-02 15:04:05") + " UTC")
	}
	fmt.Fprintf(&b, "\n<%s/monitors/%s|Open in Maiak>", Escape(publicURL), p.MonitorID)
	return b.String()
}

// DisplayURL is the monitor URL as shown in Slack: scheme, host and path only. The query string
// and fragment often carry tokens or API keys, and a Slack channel is a wider audience than the
// dashboard. An unparsable URL is not shown at all.
func DisplayURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
}

// FormatResolved: `:large_green_circle: *{name}* is back up after {duration}`.
func FormatResolved(p Payload) string {
	return fmt.Sprintf(":large_green_circle: *%s* is back up after %s", Escape(p.MonitorName), HumanDuration(time.Duration(p.DurationSeconds)*time.Second))
}

// FormatTLSExpiring: `:warning: TLS certificate for *{name}* expires in {n} days ({date})`.
// One day reads "1 day".
func FormatTLSExpiring(p Payload) string {
	n := *p.DaysLeft
	unit := "days"
	if n == 1 {
		unit = "day"
	}
	return fmt.Sprintf(":warning: TLS certificate for *%s* expires in %d %s (%s)",
		Escape(p.MonitorName), n, unit, p.TLSNotAfter.UTC().Format("2006-01-02"))
}

// TestMessage is the text of POST /settings/slack/test.
func TestMessage(publicURL string) string {
	return fmt.Sprintf(":white_check_mark: Test message from Maiak: alerts will be posted here. <%s|Open Maiak>", Escape(publicURL))
}

// Escape replaces the characters Slack mrkdwn treats as control characters (&, <, >), so a
// monitor name or URL cannot inject links or mentions.
func Escape(s string) string {
	return mrkdwnEscaper.Replace(s)
}

var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// HumanDuration renders d as days, hours and minutes ("2h 5m"); seconds only below a minute.
func HumanDuration(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(int(max(d, 0)/time.Second)) + "s"
	}
	total := int(d / time.Minute)
	days, hours, mins := total/(24*60), total/60%24, total%60
	var parts []string
	if days > 0 {
		parts = append(parts, strconv.Itoa(days)+"d")
	}
	if hours > 0 {
		parts = append(parts, strconv.Itoa(hours)+"h")
	}
	if mins > 0 {
		parts = append(parts, strconv.Itoa(mins)+"m")
	}
	return strings.Join(parts, " ")
}
