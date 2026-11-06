package statuspages

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReservedSlugs may not be used as a page slug: they name routes of the gateway, the renderer or
// the product (ADR-0103). The migration refuses the same list.
var ReservedSlugs = []string{
	"admin", "api", "app", "assets", "auth", "badge", "dashboard", "docs", "embed",
	"feed", "health", "healthz", "help", "login", "logout", "maiak", "metrics",
	"public", "readyz", "register", "robots", "root", "rss", "settings", "signup",
	"static", "status", "support", "system", "www",
}

var (
	slugPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,62}$`)
	accentPattern = regexp.MustCompile(`^#[0-9a-f]{6}$`)
)

// Limits of the page and component fields.
const (
	MaxTitle         = 80
	MaxDescription   = 500
	MaxLogoURL       = 500
	MaxNameLength    = 60
	MaxComponents    = 50
	MinPasswordRunes = 12
	MaxPasswordBytes = 128
)

// ValidateSlug returns a message for an unusable slug, "" for a good one.
func ValidateSlug(slug string) string {
	if !slugPattern.MatchString(slug) {
		return "must be 3 to 63 lowercase letters, digits and hyphens, starting with a letter or digit"
	}
	if slices.Contains(ReservedSlugs, slug) {
		return "is reserved; choose another"
	}
	return ""
}

// ValidateAccent normalises an accent colour to lowercase #rrggbb; "" clears it.
func ValidateAccent(v string) (string, string) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || accentPattern.MatchString(v) {
		return v, ""
	}
	return v, "must be a colour like #1f6feb"
}

// ValidAccent reports whether a stored value is safe to put into a style element.
func ValidAccent(v string) bool { return accentPattern.MatchString(v) }

// ValidateLogoURL checks a logo URL: absolute https, a host, no user info, at most 500 characters,
// printable ASCII only (so it cannot smuggle markup or break out of an attribute or a CSP source).
// "" clears it.
func ValidateLogoURL(v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	if len(v) > MaxLogoURL {
		return v, "must be at most 500 characters"
	}
	if _, ok := LogoOrigin(v); !ok {
		return v, "must be an https:// address of an image, without user name or password"
	}
	return v, ""
}

// LogoOrigin returns "https://host[:port]" of a valid logo URL: the only image source the page's
// Content-Security-Policy allows besides itself.
func LogoOrigin(v string) (string, bool) {
	for _, c := range v {
		if c <= 0x20 || c >= 0x7f || strings.ContainsRune(`"'<>\`+"`", c) {
			return "", false
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, ";,*") {
		return "", false
	}
	return "https://" + u.Host, true
}

// ValidateText trims a title, description, public name or group name and checks its length in
// characters; control characters are refused (line breaks only in the description).
func ValidateText(v string, minLen, maxLen int) (string, string) {
	v = strings.TrimSpace(v)
	n := utf8.RuneCountInString(v)
	if n < minLen || n > maxLen {
		if minLen > 0 {
			return v, "must be " + strconv.Itoa(minLen) + " to " + strconv.Itoa(maxLen) + " characters"
		}
		return v, "must be at most " + strconv.Itoa(maxLen) + " characters"
	}
	for _, r := range v {
		if unicode.IsControl(r) && (r != '\n' || maxLen != MaxDescription) {
			return v, "must not contain control characters"
		}
	}
	return v, ""
}

// ValidatePassword checks a page password: at least 12 characters, at most 128 bytes.
func ValidatePassword(v string) string {
	if utf8.RuneCountInString(v) < MinPasswordRunes {
		return "must be at least 12 characters"
	}
	if len(v) > MaxPasswordBytes {
		return "must be at most 128 bytes"
	}
	return ""
}
