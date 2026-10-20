package monitors

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxNameRunes = 80
	maxURLBytes  = 2048
	maxHostBytes = 253
)

// MVP values (ADR-0016); the database enforces them too (monitors_mvp_* constraints).
const (
	IntervalSeconds         = 60
	TimeoutSeconds          = 10
	DefaultFailureThreshold = 2
	DefaultExpectedStatus   = "2xx"
)

// blockedPrefixes are address ranges no monitor may target by literal. netip's predicates
// cover loopback, private (RFC 1918, fc00::/7), link-local, multicast and unspecified.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, includes the broadcast address
	netip.MustParsePrefix("::/96"),         // IPv4-compatible (deprecated), ::, ::1
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64: embeds an IPv4 address
}

// Host names that only resolve inside private networks or the cluster (single-label names are
// rejected separately: inside Kubernetes they resolve through the search domains).
var blockedSuffixes = []string{".localhost", ".internal", ".local", ".svc"}

var (
	hostPattern   = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)
	numericLabel  = regexp.MustCompile(`^(0x[0-9a-f]*|[0-9]+)$`)
	expectedRegex = regexp.MustCompile(`^(2xx|[1-5][0-9][0-9])$`)
)

// ValidateURL checks a monitor target and returns it normalised (lowercase scheme and host),
// or a message for the user.
//
// This is only the first, best-effort layer of SSRF defence: it rejects what is obviously
// internal when the monitor is saved. A public name can still resolve to a private address
// (or be rebound later), so the authoritative check is the connect-time IP validation in the
// checks package (B-09, ADR-0011), which must never rely on this function.
func ValidateURL(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "is required"
	}
	if len(raw) > maxURLBytes {
		return "", "must be at most 2048 bytes"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "is not a valid URL"
	}
	// url.Parse lowercases the scheme, so HTTPS:// is accepted.
	if u.Scheme != "https" {
		return "", "must use https"
	}
	if u.Opaque != "" || u.Host == "" {
		return "", "must have a host"
	}
	if u.User != nil {
		return "", "must not contain a user name or password"
	}
	if p := u.Port(); p != "" && p != "443" {
		return "", "must use port 443"
	}
	host := u.Hostname()
	if msg := validateHost(host); msg != "" {
		return "", msg
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), ""
}

func validateHost(host string) string {
	if host == "" {
		return "must have a host"
	}
	for _, r := range host {
		if r >= utf8.RuneSelf {
			return "host must be ASCII; write internationalised domain names in their xn-- form"
		}
	}
	host = strings.ToLower(host)

	// IPv6 literal (url.Hostname strips the brackets).
	if strings.Contains(host, ":") {
		a, err := netip.ParseAddr(host)
		if err != nil || a.Zone() != "" {
			return "host is not a valid IPv6 address"
		}
		return checkAddr(a)
	}

	// A trailing dot makes the name absolute; it changes nothing for the checks below.
	name := strings.TrimSuffix(host, ".")
	if name == "" || len(name) > maxHostBytes || !hostPattern.MatchString(name) {
		return "host is not a valid domain name"
	}
	labels := strings.Split(name, ".")
	// Resolvers accept IPv4 in decimal (2130706433), octal (0177.0.0.1), hex (0x7f.1) and short
	// (127.1) forms. A name whose last label is numeric is an address, so only the canonical
	// dotted-decimal form is accepted and then checked like any other address.
	if numericLabel.MatchString(labels[len(labels)-1]) {
		a, err := netip.ParseAddr(name)
		if err != nil || !a.Is4() {
			return "IP addresses must be written as four decimal numbers (a.b.c.d)"
		}
		return checkAddr(a)
	}
	for _, l := range labels {
		if len(l) > 63 {
			return "host is not a valid domain name"
		}
	}
	if len(labels) == 1 || name == "localhost" {
		return "host must be a public domain name"
	}
	for _, s := range blockedSuffixes {
		if strings.HasSuffix(name, s) {
			return "host must be a public domain name"
		}
	}
	return ""
}

func checkAddr(a netip.Addr) string {
	if a.Is4In6() {
		return "IPv4-mapped IPv6 addresses are not allowed"
	}
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return "host must be a public address"
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return "host must be a public address"
		}
	}
	return ""
}

// ValidateName trims a monitor name and checks it, returning a message for the user.
func ValidateName(raw string) (string, string) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", "is required"
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		return "", "must be at most 80 characters"
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", "must not contain control characters"
		}
	}
	return name, ""
}

// ValidateExpectedStatus accepts "2xx" or one status code from 100 to 599.
func ValidateExpectedStatus(s string) string {
	if !expectedRegex.MatchString(s) {
		return `must be "2xx" or a status code from 100 to 599`
	}
	return ""
}
