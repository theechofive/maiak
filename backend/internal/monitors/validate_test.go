package monitors

import (
	"strings"
	"testing"
)

func TestValidateURL(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want string // normalised URL when ok
	}{
		{"https://example.com", true, "https://example.com"},
		{"https://example.com/health?x=1#top", true, "https://example.com/health?x=1#top"},
		{"  https://example.com/  ", true, "https://example.com/"},
		{"HTTPS://Example.COM/Path", true, "https://example.com/Path"},
		{"https://example.com:443/", true, "https://example.com:443/"},
		{"https://example.com./", true, "https://example.com./"},
		{"https://xn--exmple-cua.com/", true, "https://xn--exmple-cua.com/"},
		{"https://8.8.8.8/", true, "https://8.8.8.8/"},
		{"https://[2001:4860:4860::8888]/", true, "https://[2001:4860:4860::8888]/"},
		{"https://under_score.example.com/", true, "https://under_score.example.com/"},

		// Scheme, port, credentials, shape.
		{"", false, ""},
		{"http://example.com", false, ""},
		{"ftp://example.com", false, ""},
		{"example.com", false, ""},
		{"https:example.com", false, ""},
		{"https:///path", false, ""},
		{"https://example.com:8443/", false, ""},
		{"https://example.com:80/", false, ""},
		{"https://user:pass@example.com/", false, ""},
		{"https://user@example.com/", false, ""},
		{"https://example.com\x00/", false, ""},
		{"https://exa mple.com/", false, ""},
		{"https://" + strings.Repeat("a", 2040) + ".com/", false, ""},

		// Internal names.
		{"https://localhost/", false, ""},
		{"https://LOCALHOST./", false, ""},
		{"https://app.localhost/", false, ""},
		{"https://metadata.google.internal/", false, ""},
		{"https://printer.local/", false, ""},
		{"https://maiak-api.maiak.svc/", false, ""},
		{"https://kubernetes.default.svc.cluster.local/", false, ""},
		{"https://intranet/", false, ""},
		{"https://%6c%6f%63%61%6c%68%6f%73%74/", false, ""},

		// IPv4 literals in blocked ranges and non-canonical forms.
		{"https://127.0.0.1/", false, ""},
		{"https://127.0.0.1./", false, ""},
		{"https://10.1.2.3/", false, ""},
		{"https://172.16.0.1/", false, ""},
		{"https://192.168.1.1/", false, ""},
		{"https://169.254.169.254/", false, ""},
		{"https://100.64.0.1/", false, ""},
		{"https://0.0.0.0/", false, ""},
		{"https://0.1.2.3/", false, ""},
		{"https://224.0.0.1/", false, ""},
		{"https://255.255.255.255/", false, ""},
		{"https://2130706433/", false, ""}, // decimal 127.0.0.1
		{"https://0177.0.0.1/", false, ""}, // octal
		{"https://0x7f.0.0.1/", false, ""}, // hex octet
		{"https://0x7f000001/", false, ""}, // hex
		{"https://127.1/", false, ""},      // short form
		{"https://134744072/", false, ""},  // decimal 8.8.8.8: public, but not canonical
		{"https://08.8.8.8/", false, ""},   // leading zero
		{"https://%31%32%37.0.0.1/", false, ""},

		// IPv6 literals.
		{"https://[::1]/", false, ""},
		{"https://[::]/", false, ""},
		{"https://[fc00::1]/", false, ""},
		{"https://[fd12:3456::1]/", false, ""},
		{"https://[fe80::1]/", false, ""},
		{"https://[fe80::1%25eth0]/", false, ""},
		{"https://[::ffff:127.0.0.1]/", false, ""},
		{"https://[::ffff:8.8.8.8]/", false, ""}, // any IPv4-mapped address
		{"https://[::127.0.0.1]/", false, ""},    // IPv4-compatible
		{"https://[64:ff9b::a00:1]/", false, ""}, // NAT64 of 10.0.0.1
		{"https://[ff02::1]/", false, ""},
		{"https://[::1]:443/", false, ""},

		// Unicode hosts (homographs, fullwidth digits) must use their xn-- form.
		{"https://exämple.com/", false, ""},
		{"https://ｌｏｃａｌｈｏｓｔ/", false, ""},
		{"https://１２７.0.0.1/", false, ""},
	}
	for _, c := range cases {
		got, msg := ValidateURL(c.in)
		if c.ok && (msg != "" || got != c.want) {
			t.Errorf("ValidateURL(%q) = %q, %q; want %q accepted", c.in, got, msg, c.want)
		}
		if !c.ok && msg == "" {
			t.Errorf("ValidateURL(%q) = %q accepted; want rejected", c.in, got)
		}
	}
}

func TestValidateName(t *testing.T) {
	if got, msg := ValidateName("  Website  "); msg != "" || got != "Website" {
		t.Fatalf("got %q, %q", got, msg)
	}
	if _, msg := ValidateName(strings.Repeat("ж", 80)); msg != "" {
		t.Fatalf("80 characters rejected: %s", msg)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("ж", 81), "a\nb", "a\x00b"} {
		if _, msg := ValidateName(bad); msg == "" {
			t.Errorf("ValidateName(%q) accepted", bad)
		}
	}
}

func TestValidateExpectedStatus(t *testing.T) {
	for _, ok := range []string{"2xx", "100", "200", "204", "301", "418", "599"} {
		if msg := ValidateExpectedStatus(ok); msg != "" {
			t.Errorf("%q rejected: %s", ok, msg)
		}
	}
	for _, bad := range []string{"", "2XX", "3xx", "99", "600", "099", "20", "2000", " 200", "200 "} {
		if msg := ValidateExpectedStatus(bad); msg == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}
