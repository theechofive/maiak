package auth

import (
	"context"
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
)

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"owner@example.com":        "owner@example.com",
		"  User@Example.COM\t":     "user@example.com",
		"first.last+tag@sub.ex.io": "first.last+tag@sub.ex.io",
		"a_b-c@x.co":               "a_b-c@x.co",
		"a@b.c":                    "a@b.c",
		strings.Repeat("a", 64) + "@" + strings.Repeat("d", 185) + ".com": strings.Repeat("a", 64) + "@" + strings.Repeat("d", 185) + ".com",
	} {
		got, msg := normalizeEmail(in)
		if msg != "" || got != want {
			t.Errorf("%q: got %q, %q; want %q", in, got, msg, want)
		}
	}
	for _, in := range []string{
		"", "a@", "@example.com", "ab", "owner", "owner@example", "owner@@example.com", "a@b@example.com",
		"owner@.example.com", "owner@example.com.", "owner@example..com",
		"o wner@example.com", "owner@exa mple.com", "owner\n@example.com", "owner@example.com\x00",
		"üser@example.com", "owner@exämple.com", "Kelvin@example.com", "o'brien@example.com", "a!b@example.com",
		strings.Repeat("a", 64) + "@" + strings.Repeat("d", 186) + ".com", // 255 characters
	} {
		if got, msg := normalizeEmail(in); msg == "" {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestValidateRegistration(t *testing.T) {
	fields := func(err error) map[string]string {
		var verr *validation.Error
		if !errors.As(err, &verr) {
			return nil
		}
		m := map[string]string{}
		for _, f := range verr.Fields {
			m[f.Field] = f.Message
		}
		return m
	}
	email, err := validateRegistration(" Owner@Example.com ", "correct horse battery")
	if err != nil || email != "owner@example.com" {
		t.Fatalf("valid input: %q, %v", email, err)
	}
	// Code points are counted, not bytes: 12 two-byte runes pass, 11 do not.
	if _, err := validateRegistration("a@example.com", strings.Repeat("é", 12)); err != nil {
		t.Fatalf("12 code points rejected: %v", err)
	}
	if _, err := validateRegistration("a@example.com", strings.Repeat("ab", 64)); err != nil {
		t.Fatalf("128 characters rejected: %v", err)
	}
	for name, c := range map[string]struct{ email, password, field, msg string }{
		"short password":        {"a@example.com", "elevenchars", "password", "must be at least 12 characters"},
		"11 code points":        {"a@example.com", strings.Repeat("é", 11), "password", "must be at least 12 characters"},
		"129 characters":        {"a@example.com", strings.Repeat("x", 129), "password", "must be at most 128 characters"},
		"password equals email": {"Long.Address@Example.com", "long.address@example.COM", "password", "must not be the same as the email"},
		"bad email":             {"not-an-email", "correct horse battery", "email", "must contain exactly one @"},
	} {
		got := fields(func() error { _, err := validateRegistration(c.email, c.password); return err }())
		if got[c.field] != c.msg {
			t.Errorf("%s: field errors %v, want %s=%q", name, got, c.field, c.msg)
		}
	}
	// Both fields are reported at once.
	if got := fields(func() error { _, err := validateRegistration("x", "short"); return err }()); len(got) != 2 {
		t.Fatalf("field errors = %v, want email and password", got)
	}
}

func TestTenantSlugAndName(t *testing.T) {
	// Random only: "t-" and 10 base32 characters, never "default", never a part of the email.
	slugRE := regexp.MustCompile(`^t-[a-z2-7]{10}$`)
	seen := map[string]bool{}
	for range 1000 {
		slug, err := tenantSlug()
		if err != nil {
			t.Fatal(err)
		}
		if !slugRE.MatchString(slug) || slug == "default" || seen[slug] {
			t.Fatalf("slug %q: want a fresh match of %s", slug, slugRE)
		}
		seen[slug] = true
	}
	if got := tenantName("owner@example.com"); got != "example.com" {
		t.Fatalf("name = %q", got)
	}
	if got := tenantName("o@" + strings.Repeat("d", 201) + ".com"); got != defaultTenantName {
		t.Fatalf("long domain name = %q", got)
	}
}

// A full argon2 queue answers a registration with RateLimitedError before any database work,
// and the per-IP attempt is refunded.
func TestRegisterHashQueueFullIsRateLimited(t *testing.T) {
	s, err := NewService(nil, Options{HashQueueWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s.dummy = &crypto.ParsedHash{}
	for range maxConcurrentHashes {
		s.hashSem <- struct{}{}
	}
	ip := netip.MustParseAddr("192.0.2.1")
	_, err = s.Register(context.Background(), RegisterInput{Email: "a@example.com", Password: "a long enough password", IP: ip})
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Fatalf("err = %v, want RateLimitedError", err)
	}
	if ok, _ := s.ipLimit.Reserve(limiterIPKey(ip)); !ok {
		t.Fatal("attempt not refunded")
	}
}
