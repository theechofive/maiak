package public

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/statuspages"
)

const publicURL = "https://maiak.example.com"

// fakeSource serves stored payloads like status_page_snapshot would, and counts lookups.
type fakeSource struct {
	mu      sync.Mutex
	pages   map[string]stored
	lookups int
}

type stored struct {
	payload string
	hash    string
}

func (f *fakeSource) Lookup(_ context.Context, slug string) (Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	p, ok := f.pages[slug]
	if !ok {
		return Entry{}, nil
	}
	var hash *string
	if p.hash != "" {
		hash = &p.hash
	}
	return newEntry([]byte(p.payload), "0123456789abcdef0123456789abcdef", hash)
}

const xss = `<script>alert(1)</script>"'><img src=x onerror=alert(2)></style>`

func payload(t *testing.T, mutate func(*statuspages.Snapshot)) string {
	t.Helper()
	logo, accent, group := "https://cdn.example.com/logo.png", "#1f6feb", "Core "+xss
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	start := now.Add(-90 * time.Minute)
	resolved := now.Add(-time.Hour)
	days := make([]statuspages.Day, 90)
	for i, d := range statuspages.HistoryWindow(now) {
		days[i] = statuspages.Day{Date: d.Format(time.DateOnly), Status: statuspages.NoData}
	}
	up := 99.5
	days[89] = statuspages.Day{Date: "2026-11-06", Status: statuspages.Degraded, Uptime: &up}
	s := statuspages.Snapshot{Version: 1, Page: statuspages.PublicPage{Slug: "acme", Title: "Acme " + xss, Description: "Line one\n" + xss, LogoURL: &logo, AccentColor: &accent},
		Status: statuspages.PartialOutage, GeneratedAt: now,
		Components: []statuspages.Component{
			{Name: "API " + xss, Group: &group, Status: statuspages.Operational, Uptime: &up, Days: days},
			{Name: "Database", Group: &group, Status: statuspages.Outage, Days: days},
			{Name: "Website", Status: statuspages.NoData, Days: days},
		},
		ActiveIncidents: []statuspages.Incident{{ID: "aaaaaaaaaaaaaaaaaaaaaaaa", Title: "Database is unavailable", Components: []string{"Database"}, StartedAt: now.Add(-10 * time.Minute), DurationSeconds: 600}},
		PastIncidents:   []statuspages.Incident{{ID: "bbbbbbbbbbbbbbbbbbbbbbbb", Title: "API " + xss + " is unavailable", Components: []string{"API"}, StartedAt: start, ResolvedAt: &resolved, DurationSeconds: 1800}},
	}
	if mutate != nil {
		mutate(&s)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type harness struct {
	t   *testing.T
	src *fakeSource
	srv *httptest.Server
	clk *clock.Fake
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	src := &fakeSource{pages: map[string]stored{"acme": {payload: payload(t, nil)}}}
	h := NewHandler(Config{PublicURL: publicURL, TrustedProxyHops: 1, CookieSecure: true},
		Deps{Source: src, Cookies: NewCookieSigner([]byte("0123456789abcdef0123456789abcdef")), Clock: clk})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &harness{t: t, src: src, srv: srv, clk: clk}
}

type resp struct {
	code   int
	header http.Header
	body   string
}

func (h *harness) do(method, path string, body string, hdr map[string]string) resp {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{code: r.StatusCode, header: r.Header, body: string(b)}
}

func (h *harness) get(path string) resp { return h.do(http.MethodGet, path, "", nil) }

// comparable strips the per-request header so two 404s can be compared exactly.
func comparable(r resp) string {
	hd := r.header.Clone()
	hd.Del("X-Request-Id")
	hd.Del("Date")
	var b strings.Builder
	_ = hd.Write(&b)
	return b.String() + "\n" + r.body
}

func TestNotFoundIsIdentical(t *testing.T) {
	h := newHarness(t)
	h.src.pages["locked"] = stored{payload: payload(t, nil), hash: "$argon2id$x"}
	base := h.get("/status/nope")
	if base.code != http.StatusNotFound || !strings.Contains(base.body, "Status page not found") || base.header.Get("Cache-Control") != "public, max-age=30" {
		t.Fatalf("404: %d %v", base.code, base.header)
	}
	for _, path := range []string{
		"/status/unpublished-or-deleted", "/status/ab", "/status/UPPER", "/status/acme/nothing", "/status/acme.xml",
		"/status/locked.json", "/status/locked/feed.rss", "/status/locked/badge", "/status/locked/embed.js",
		"/status/", "/status", "/status/a%2Fb", "/other",
	} {
		if r := h.get(path); comparable(r) != comparable(base) {
			t.Errorf("%s differs from an unknown page:\n%s\n---\n%s", path, comparable(r), comparable(base))
		}
	}
	if r := h.do(http.MethodPost, "/status/nope/unlock", "password=x", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); comparable(r) != comparable(base) {
		t.Errorf("unlock of an unknown page differs")
	}
	if r := h.do(http.MethodDelete, "/status/acme", "", nil); comparable(r) != comparable(base) {
		t.Errorf("wrong method differs")
	}
}

var nonceRe = regexp.MustCompile(`'nonce-([0-9a-f]{32})'`)

func TestPageRendersEscapedWithStrictPolicy(t *testing.T) {
	h := newHarness(t)
	r := h.get("/status/acme")
	if r.code != http.StatusOK || r.header.Get("Content-Type") != "text/html; charset=utf-8" || r.header.Get("Cache-Control") != "public, max-age=30" {
		t.Fatalf("page: %d %v", r.code, r.header)
	}
	for _, want := range []string{"Partial outage", "Core ", "API ", "Database is unavailable", "Outage", "No data", "Since <time", "resolved after 30 min",
		"99.50 % uptime", `href="/status/acme/feed.rss"`, `href="/status/acme.json"`, `referrerpolicy="no-referrer"`, "Line one\n"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Every user field is escaped: no raw tag, attribute break-out or closing style anywhere.
	for _, bad := range []string{"<script>", "<img src=x", "</style>\"", `"'>`} {
		if strings.Contains(r.body, bad) {
			t.Errorf("unescaped %q in page", bad)
		}
	}
	if strings.Count(r.body, "&lt;script&gt;alert(1)&lt;/script&gt;") < 4 {
		t.Errorf("escaped payload not found in title, description, group, component and incident")
	}
	csp := r.header.Get("Content-Security-Policy")
	m := nonceRe.FindStringSubmatch(csp)
	if m == nil || !strings.Contains(r.body, `<style nonce="`+m[1]+`">:root{--accent:#1f6feb}</style>`) {
		t.Fatalf("nonce'd accent style missing; csp %q", csp)
	}
	for _, want := range []string{"default-src 'none'", "style-src 'self' 'nonce-", "img-src https://cdn.example.com;", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "script-src") || strings.Contains(csp, "unsafe") || strings.Contains(csp, "img-src https:;") {
		t.Errorf("CSP too wide: %s", csp)
	}
	if strings.Contains(r.body, "<script") || strings.Contains(r.body, " style=") {
		t.Error("the page carries a script or an inline style attribute")
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
		if r.header.Get(k) != v {
			t.Errorf("%s = %q", k, r.header.Get(k))
		}
	}
	if r.header.Get("Set-Cookie") != "" {
		t.Error("a public page sets a cookie")
	}
	// A fresh nonce per response.
	if m2 := nonceRe.FindStringSubmatch(h.get("/status/acme").header.Get("Content-Security-Policy")); m2 == nil || m2[1] == m[1] {
		t.Error("nonce reused")
	}
	// Bars carry classes only; 90 per component.
	if n := strings.Count(r.body, `<li class="s-`); n != 270 {
		t.Errorf("%d bars, want 270", n)
	}
}

func TestUnsafeBrandingInASnapshotIsDropped(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ logo, accent string }{
		{"javascript:alert(1)", "red;}body{background:url(https://evil.example/x)"},
		{"http://cdn.example.com/x.png", "#FF0000"},
		{"https://a.example.com/x.png\" onerror=\"x", "#12345"},
		{"https://*.example.com/x.png", "#123456;"},
	} {
		h.src.pages["acme"] = stored{payload: payload(t, func(s *statuspages.Snapshot) { s.Page.LogoURL, s.Page.AccentColor = &c.logo, &c.accent })}
		h.clk.Advance(CacheTTL)
		r := h.get("/status/acme")
		if strings.Contains(r.body, "<img") || strings.Contains(r.body, "--accent") || strings.Contains(r.header.Get("Content-Security-Policy"), "img-src https") {
			t.Errorf("unsafe branding rendered for %+v:\n%s", c, r.header.Get("Content-Security-Policy"))
		}
		if !strings.Contains(r.header.Get("Content-Security-Policy"), "img-src 'none'") {
			t.Errorf("img-src not closed: %s", r.header.Get("Content-Security-Policy"))
		}
	}
}

func TestJSONIsAllowListedAndCached(t *testing.T) {
	h := newHarness(t)
	// A stored payload with fields outside the allow-list (a bug elsewhere) never reaches visitors.
	var raw map[string]any
	_ = json.Unmarshal([]byte(payload(t, nil)), &raw)
	raw["tenantId"] = "leak"
	raw["components"].([]any)[0].(map[string]any)["monitorUrl"] = "https://internal.example.net/x"
	b, _ := json.Marshal(raw)
	h.src.pages["acme"] = stored{payload: string(b)}
	r := h.get("/status/acme.json")
	if r.code != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") || strings.Contains(r.body, "leak") || strings.Contains(r.body, "internal.example.net") {
		t.Fatalf("json: %d %s", r.code, r.body)
	}
	if r.header.Get("ETag") != `"0123456789abcdef0123456789abcdef"` || r.header.Get("Cache-Control") != "public, max-age=30" || r.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("json headers: %v", r.header)
	}
	if r := h.do(http.MethodGet, "/status/acme.json", "", map[string]string{"If-None-Match": `"0123456789abcdef0123456789abcdef"`}); r.code != http.StatusNotModified || r.body != "" {
		t.Fatalf("conditional: %d", r.code)
	}
	// One lookup for many requests within the TTL, a new one after it.
	n := h.src.lookups
	for range 5 {
		h.get("/status/acme.json")
	}
	if h.src.lookups != n {
		t.Fatalf("lookups %d -> %d within the TTL", n, h.src.lookups)
	}
	h.clk.Advance(CacheTTL)
	h.get("/status/acme.json")
	if h.src.lookups != n+1 {
		t.Fatal("cache did not expire")
	}
	// Unknown slugs are cached too, so random slugs do not each reach the database twice.
	h.get("/status/random-slug")
	h.get("/status/random-slug")
	if h.src.lookups != n+2 {
		t.Fatalf("negative answer not cached: %d", h.src.lookups-n)
	}
}

func TestRSSBadgeAndEmbed(t *testing.T) {
	h := newHarness(t)
	r := h.get("/status/acme/feed.rss")
	if r.code != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("rss: %d %v", r.code, r.header)
	}
	var feed rss
	if err := xml.Unmarshal([]byte(r.body), &feed); err != nil {
		t.Fatalf("rss is not XML: %v", err)
	}
	if len(feed.Channel.Items) != 3 || feed.Channel.Items[1].Title != "API "+xss+" is unavailable" || feed.Channel.Items[2].Title != "Resolved: API "+xss+" is unavailable" ||
		feed.Channel.Items[0].GUID.Value != "aaaaaaaaaaaaaaaaaaaaaaaa-opened" || feed.Channel.Items[0].Link != publicURL+"/status/acme#incident-aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("items: %+v", feed.Channel.Items)
	}
	if strings.Contains(r.body, "<script>") {
		t.Fatal("rss not escaped")
	}

	b := h.get("/status/acme/badge")
	csp := b.header.Get("Content-Security-Policy")
	if b.code != http.StatusOK || !strings.Contains(csp, "frame-ancestors *") || b.header.Get("X-Frame-Options") != "" || !strings.Contains(b.body, "Partial outage") ||
		!strings.Contains(b.body, `href="`+publicURL+`/status/acme"`) || !strings.Contains(b.body, `rel="noopener noreferrer"`) || strings.Contains(csp, "script") {
		t.Fatalf("badge: %d %s\n%s", b.code, csp, b.body)
	}
	h.src.pages["acme"] = stored{payload: payload(t, func(s *statuspages.Snapshot) { s.Status = statuspages.Operational })}
	h.clk.Advance(CacheTTL)
	if b := h.get("/status/acme/badge"); !strings.Contains(b.body, "All systems operational") {
		t.Fatal("badge text")
	}

	e := h.get("/status/acme/embed.js")
	if e.code != http.StatusOK || !strings.HasPrefix(e.header.Get("Content-Type"), "text/javascript") || e.header.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Fatalf("embed: %d %v", e.code, e.header)
	}
	if !strings.Contains(e.body, `"`+publicURL+`/status/acme/badge"`) || strings.Contains(e.body, "<script>") || strings.Contains(e.body, "</") {
		t.Fatalf("embed script: %s", e.body)
	}
	if !strings.Contains(e.body, `\u003cscript\u003e`) {
		t.Fatalf("title not JSON-escaped in the script: %s", e.body)
	}
}

func TestPasswordFlow(t *testing.T) {
	h := newHarness(t)
	hash, err := crypto.HashPassword("correct horse battery", crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	h.src.pages["acme"] = stored{payload: payload(t, nil), hash: hash}

	lock := h.get("/status/acme")
	if lock.code != http.StatusOK || !strings.Contains(lock.body, `action="/status/acme/unlock"`) || strings.Contains(lock.body, "Database is unavailable") ||
		lock.header.Get("Cache-Control") != "private, no-store" || !strings.Contains(lock.body, `name="robots" content="noindex`) {
		t.Fatalf("lock page: %d %v", lock.code, lock.header)
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": publicURL}
	wrong := h.do(http.MethodPost, "/status/acme/unlock", "password=wrong+password", form)
	if wrong.code != http.StatusUnauthorized || !strings.Contains(wrong.body, "not correct") || wrong.header.Get("Set-Cookie") != "" {
		t.Fatalf("wrong password: %d", wrong.code)
	}
	cross := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "https://evil.example"}
	if r := h.do(http.MethodPost, "/status/acme/unlock", "password="+url.QueryEscape("correct horse battery"), cross); r.code != http.StatusNotFound {
		t.Fatalf("cross-origin unlock: %d", r.code)
	}
	ok := h.do(http.MethodPost, "/status/acme/unlock", "password="+url.QueryEscape("correct horse battery"), form)
	if ok.code != http.StatusSeeOther || ok.header.Get("Location") != "/status/acme" {
		t.Fatalf("unlock: %d %v", ok.code, ok.header)
	}
	c := (&http.Response{Header: ok.header}).Cookies()
	if len(c) != 1 || c[0].Name != CookieName || c[0].Path != "/status/acme" || !c[0].HttpOnly || !c[0].Secure || c[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie: %+v", c)
	}
	cookie := map[string]string{"Cookie": CookieName + "=" + c[0].Value}
	page := h.do(http.MethodGet, "/status/acme", "", cookie)
	if !strings.Contains(page.body, "Database is unavailable") || page.header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("unlocked page: %d %v", page.code, page.header)
	}
	// The cookie does not open the JSON, feed or badge, nor another page, nor survive a new password.
	if r := h.do(http.MethodGet, "/status/acme.json", "", cookie); r.code != http.StatusNotFound {
		t.Fatalf("json of a protected page: %d", r.code)
	}
	h.src.pages["other"] = stored{payload: payload(t, nil), hash: hash}
	if r := h.do(http.MethodGet, "/status/other", "", cookie); strings.Contains(r.body, "Database is unavailable") {
		t.Fatal("cookie of one page opens another")
	}
	newHash, _ := crypto.HashPassword("a brand new password", crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	h.src.pages["acme"] = stored{payload: payload(t, nil), hash: newHash}
	h.clk.Advance(CacheTTL)
	if r := h.do(http.MethodGet, "/status/acme", "", cookie); strings.Contains(r.body, "Database is unavailable") {
		t.Fatal("cookie survived a password change")
	}
	h.clk.Advance(CookieTTL)
	if NewCookieSigner([]byte("0123456789abcdef0123456789abcdef")).Valid(c[0].Value, "acme", hash, h.clk.Now()) {
		t.Fatal("cookie valid after expiry")
	}

	// Attempts are limited per client IP.
	limited := false
	for range 12 {
		if r := h.do(http.MethodPost, "/status/acme/unlock", "password=guess+guess+guess", form); r.code == http.StatusTooManyRequests {
			limited = r.header.Get("Retry-After") != ""
			break
		}
	}
	if !limited {
		t.Fatal("password attempts are not limited")
	}
}

func TestViewRateLimit(t *testing.T) {
	h := newHarness(t)
	var last resp
	for range viewBurst + 1 {
		last = h.get("/status/acme")
	}
	if last.code != http.StatusTooManyRequests || last.header.Get("Retry-After") == "" {
		t.Fatalf("after the burst: %d", last.code)
	}
	// Another client is not affected.
	if r := h.do(http.MethodGet, "/status/acme", "", map[string]string{"X-Forwarded-For": "198.51.100.1"}); r.code != http.StatusOK {
		t.Fatalf("other client: %d", r.code)
	}
	h.clk.Advance(time.Second)
	if r := h.get("/status/acme"); r.code != http.StatusOK {
		t.Fatalf("after refill: %d", r.code)
	}
}

func TestStylesheetAndFormatting(t *testing.T) {
	h := newHarness(t)
	r := h.get(cssURL())
	if r.code != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "text/css") || !strings.Contains(r.body, "light-dark(") {
		t.Fatalf("css: %d", r.code)
	}
	if formatDuration(30) != "less than a minute" || formatDuration(3720) != "1 h 2 min" || formatDuration(3*86400) != "3 days" || formatPercent(99.996) != "99.99 %" || formatPercent(100) != "100.00 %" {
		t.Fatal("formatting")
	}
	if formatTime(time.Date(2026, 11, 6, 14, 3, 0, 0, time.FixedZone("CET", 3600))) != "6 Nov 2026, 13:03 UTC" {
		t.Fatal(formatTime(time.Date(2026, 11, 6, 14, 3, 0, 0, time.FixedZone("CET", 3600))))
	}
}

func TestLimitKeyGroupsIPv6By64(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":                      "203.0.113.9",
		"::ffff:203.0.113.9":               "203.0.113.9",
		"2001:db8:1:2:aaaa:bbbb:cccc:dddd": "2001:db8:1:2::/64",
		"2001:db8:1:2::1":                  "2001:db8:1:2::/64",
	} {
		if got := limitKey(netip.MustParseAddr(in)); got != want {
			t.Errorf("limitKey(%s) = %s, want %s", in, got, want)
		}
	}
	// Two addresses of one /64 share the view limit; another /64 does not.
	h := newHarness(t)
	var last resp
	for i := range viewBurst + 1 {
		last = h.do(http.MethodGet, "/status/acme", "", map[string]string{"X-Forwarded-For": fmt.Sprintf("2001:db8:1:2::%x", i+1)})
	}
	if last.code != http.StatusTooManyRequests {
		t.Fatalf("addresses of one /64 are not limited together: %d", last.code)
	}
	if r := h.do(http.MethodGet, "/status/acme", "", map[string]string{"X-Forwarded-For": "2001:db8:1:3::1"}); r.code != http.StatusOK {
		t.Fatalf("another /64: %d", r.code)
	}
}

// An attacker spreading wrong passwords over many addresses exhausts the page's failure budget,
// but cannot lock out a visitor who knows the password: only failures count against the page, and
// every client keeps a small reserve of its own.
func TestPageFailuresDoNotLockOutVisitors(t *testing.T) {
	h := newHarness(t)
	hash, err := crypto.HashPassword("correct horse battery", crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	h.src.pages["acme"] = stored{payload: payload(t, nil), hash: hash}
	attempt := func(ip, password string) int {
		return h.do(http.MethodPost, "/status/acme/unlock", "password="+url.QueryEscape(password),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": publicURL, "X-Forwarded-For": ip}).code
	}
	// failuresPerPage wrong passwords from 50 attackers (2 each, below the per-client limit).
	for i := range failuresPerPage {
		if c := attempt(fmt.Sprintf("198.51.%d.%d", i/2/250, i/2%250+1), "wrong guess"); c != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	// An attacker that already failed and exhausts its reserve is refused now.
	for range reservePerVisitor {
		attempt("198.51.0.1", "wrong guess")
	}
	if c := attempt("198.51.0.1", "wrong guess"); c != http.StatusTooManyRequests {
		t.Fatalf("attacker after the page budget and its reserve: %d", c)
	}
	// A visitor with the password still gets in, and a visitor's typo is still answered.
	if c := attempt("192.0.2.50", "typo typo typo"); c != http.StatusUnauthorized {
		t.Fatalf("visitor typo: %d", c)
	}
	if c := attempt("192.0.2.50", "correct horse battery"); c != http.StatusSeeOther {
		t.Fatalf("visitor with the password: %d", c)
	}
}

func TestLimiterEvictsLeastRecentlyUsed(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	l := NewLimiter(0.001, 1, 3, clk)
	l.Allow("a")
	l.Allow("b")
	l.Allow("c")
	l.Allow("a") // a is now the most recent; b the oldest
	l.Allow("d") // evicts b only
	if l.Len() != 3 {
		t.Fatalf("len %d", l.Len())
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a was reset by the eviction")
	}
	if ok, _ := l.Allow("c"); ok {
		t.Fatal("c was reset by the eviction")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b should have been evicted (fresh bucket)")
	}
	// Take goes below zero but not without bound; Has does not spend.
	l2 := NewLimiter(1, 2, 10, clk)
	for range 10 {
		l2.Take("x")
	}
	if ok, _ := l2.Has("x"); ok {
		t.Fatal("Has after Take")
	}
	clk.Advance(5 * time.Second)
	if ok, _ := l2.Has("x"); !ok {
		t.Fatal("bucket did not refill from its floor of -burst")
	}
	if ok, _ := l2.Has("x"); !ok {
		t.Fatal("Has spent a token")
	}
}
