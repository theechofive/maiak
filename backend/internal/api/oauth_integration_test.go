package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/oauth"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/projects"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// stubProvider is an oauth.Provider without HTTP: the test decides which identity a code yields
// (the HTTP providers are tested against a fake server in internal/oauth).
type stubProvider struct {
	name string
	mu   sync.Mutex
	ids  map[string]oauth.Identity
	errs map[string]error
	seen []oauth.TokenRequest
}

func newStub(name string) *stubProvider {
	return &stubProvider{name: name, ids: map[string]oauth.Identity{}, errs: map[string]error{}}
}

func (p *stubProvider) Name() string        { return p.name }
func (p *stubProvider) DisplayName() string { return strings.ToUpper(p.name[:1]) + p.name[1:] }

func (p *stubProvider) AuthCodeURL(_ context.Context, r oauth.AuthRequest) (string, error) {
	return "https://idp.example/" + p.name + "/authorize?" + url.Values{"state": {r.State}, "redirect_uri": {r.RedirectURI},
		"code_challenge": {r.CodeChallenge}}.Encode(), nil
}

func (p *stubProvider) Exchange(_ context.Context, r oauth.TokenRequest) (oauth.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, r)
	if err := p.errs[r.Code]; err != nil {
		return oauth.Identity{}, err
	}
	id, ok := p.ids[r.Code]
	if !ok {
		return oauth.Identity{}, oauth.ErrProvider
	}
	return id, nil
}

func (p *stubProvider) on(code string, id oauth.Identity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids[code] = id
}

type oauthServer struct {
	*client
	google, github *stubProvider
	noRedirect     *http.Client
}

func newOAuthServer(t *testing.T, env *pgtest.Env, opts auth.Options, cfg api.Config) *oauthServer {
	t.Helper()
	ctx := context.Background()
	pool := env.Pool(t, env.APIURL, 8)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts.SessionTTL, opts.Logger = time.Hour, log
	if opts.InviteLimiter == nil {
		opts.InviteLimiter = auth.NewLimiter(1000, time.Minute, 1000, clock.System{})
	}
	svc, err := auth.NewService(pool, opts)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := crypto.HashPassword(adminPassword, testHashParams)
	if err := svc.BootstrapAdmin(ctx, adminLogin, hash); err != nil {
		t.Fatal(err)
	}
	g, gh := newStub("google"), newStub("github")
	flow, err := oauth.NewFlow([]byte(strings.Repeat("k", 32)), "https://maiak.example.com", nil, g, gh)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Version, cfg.Commit, cfg.CookieSecure, cfg.TrustedProxyHops, cfg.PublicURL = "test", "test", true, 1, "https://maiak.example.com"
	h, err := api.NewHandler(cfg, api.Deps{Auth: svc, Queries: queries.New(pool, nil), Plans: plans.NewService(pool),
		Projects: projects.NewService(pool), Logger: log, OAuth: flow})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	nr := *srv.Client()
	nr.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &oauthServer{client: &client{t: t, srv: srv}, google: g, github: gh, noRedirect: &nr}
}

// start posts the start request and returns the state cookie and the state parameter.
func (s *oauthServer) start(provider, body string, hdr map[string]string) (string, string) {
	s.t.Helper()
	r := s.do(http.MethodPost, "/api/v1/auth/oauth/"+provider+"/start", body, hdr)
	if r.code != http.StatusOK {
		s.t.Fatalf("start %s: %d %s", provider, r.code, r.raw)
	}
	var cookie *http.Cookie
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		if c.Name == api.OAuthCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode ||
		cookie.Path != "/api/v1/auth/oauth/" || cookie.MaxAge != 600 {
		s.t.Fatalf("state cookie: %+v", cookie)
	}
	u, err := url.Parse(r.body["authorizationUrl"].(string))
	if err != nil {
		s.t.Fatal(err)
	}
	if got := u.Query().Get("redirect_uri"); got != "https://maiak.example.com/api/v1/auth/oauth/"+provider+"/callback" {
		s.t.Fatalf("redirect_uri %q", got)
	}
	if strings.Contains(r.raw, "inviteToken") || strings.Contains(u.String(), "invite") {
		s.t.Fatal("invitation token leaks into the start answer")
	}
	return cookie.Value, u.Query().Get("state")
}

type callbackResult struct {
	location string
	cookies  map[string]*http.Cookie
}

// callback calls the callback like the browser coming back from the provider.
func (s *oauthServer) callback(provider, query, stateCookie string, extra map[string]string) callbackResult {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.srv.URL+"/api/v1/auth/oauth/"+provider+"/callback?"+query, nil)
	var cookies []string
	if stateCookie != "" {
		cookies = append(cookies, api.OAuthCookie+"="+stateCookie)
	}
	for k, v := range extra {
		if k == "Cookie" {
			cookies = append(cookies, v)
			continue
		}
		req.Header.Set(k, v)
	}
	if len(cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(cookies, "; "))
	}
	if req.Header.Get("X-Forwarded-For") == "" {
		req.Header.Set("X-Forwarded-For", "198.51.100.30")
	}
	resp, err := s.noRedirect.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusSeeOther {
		s.t.Fatalf("callback: %d %s", resp.StatusCode, body)
	}
	out := callbackResult{location: resp.Header.Get("Location"), cookies: map[string]*http.Cookie{}}
	for _, c := range resp.Cookies() {
		out.cookies[c.Name] = c
	}
	if c := out.cookies[api.OAuthCookie]; c == nil || c.MaxAge >= 0 {
		s.t.Fatalf("callback did not clear the state cookie: %+v", c)
	}
	return out
}

func q(code, state string) string { return url.Values{"code": {code}, "state": {state}}.Encode() }

func TestIntegrationOAuthHTTPFlow(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apioauth"))
	s := newOAuthServer(t, env, membersOpts(), api.Config{RegistrationEnabled: true, PasswordLoginEnabled: true})
	admin := env.AdminConn(t)

	r := s.do(http.MethodGet, "/api/v1/auth/providers", "", nil)
	if r.code != http.StatusOK || !strings.Contains(r.raw, `{"id":"google","name":"Google"}`) || r.body["passwordLoginEnabled"] != true {
		t.Fatalf("providers: %d %s", r.code, r.raw)
	}
	if r := s.do(http.MethodPost, "/api/v1/auth/oauth/gitlab/start", `{}`, nil); r.code != http.StatusBadRequest {
		t.Fatalf("unknown provider: %d %s", r.code, r.raw)
	}

	// A full sign-in creates an account (registration is open) and a session.
	cookie, state := s.start("google", `{}`, nil)
	s.google.on("c1", oauth.Identity{Provider: "google", Subject: "sub-1", Email: "ada@example.com"})
	res := s.callback("google", q("c1", state), cookie, nil)
	sess := res.cookies[api.SessionCookie]
	if res.location != "/" || sess == nil || !sess.HttpOnly || sess.Value == "" {
		t.Fatalf("sign-in: %+v", res)
	}
	me := s.do(http.MethodGet, "/api/v1/auth/me", "", map[string]string{"Cookie": api.SessionCookie + "=" + sess.Value})
	if me.code != http.StatusOK || !strings.Contains(me.raw, `"login":"ada@example.com"`) {
		t.Fatalf("me: %d %s", me.code, me.raw)
	}
	// The verifier sent to the provider is the one behind the challenge of the authorization URL.
	if seen := s.google.seen[len(s.google.seen)-1]; seen.CodeVerifier == "" || seen.RedirectURI != "https://maiak.example.com/api/v1/auth/oauth/google/callback" {
		t.Fatalf("token request: %+v", seen)
	}

	// A state cookie works once per flow only in the browser that started it: no cookie, another
	// state, another provider and a replay after success all fail without reaching the provider.
	cookie, state = s.start("google", `{}`, nil)
	s.google.on("c2", oauth.Identity{Provider: "google", Subject: "sub-2", Email: "bob@example.com"})
	exchanges := len(s.google.seen)
	for name, c := range map[string]struct{ provider, query, cookie string }{
		"no cookie":      {"google", q("c2", state), ""},
		"wrong state":    {"google", q("c2", state+"x"), cookie},
		"other provider": {"github", q("c2", state), cookie},
		"forged cookie":  {"google", q("c2", state), cookie[:len(cookie)-2] + "AA"},
	} {
		if got := s.callback(c.provider, c.query, c.cookie, nil); got.location != "/login?error=state_invalid" || got.cookies[api.SessionCookie] != nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
	if len(s.google.seen) != exchanges {
		t.Fatal("a callback with an invalid state reached the provider")
	}

	// The provider's error text is never reflected.
	got := s.callback("google", "error=access_denied&error_description=%3Cscript%3E&state="+state, cookie, nil)
	if got.location != "/login?error=cancelled" {
		t.Fatalf("cancelled: %+v", got)
	}

	// No automatic link to a password account with the same email.
	reg := s.do(http.MethodPost, "/api/v1/auth/register", registerBody("victim@example.com", ownerPassword), map[string]string{"X-Forwarded-For": "198.51.100.31"})
	if reg.code != http.StatusCreated {
		t.Fatalf("register: %d %s", reg.code, reg.raw)
	}
	cookie, state = s.start("github", `{}`, nil)
	s.github.on("c3", oauth.Identity{Provider: "github", Subject: "77", Email: "victim@example.com"})
	if got := s.callback("github", q("c3", state), cookie, nil); got.location != "/login?error=account_exists" || got.cookies[api.SessionCookie] != nil {
		t.Fatalf("account_exists: %+v", got)
	}
	var n int
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM auth_identities WHERE subject = '77'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("identity linked automatically: %d %v", n, err)
	}

	// email_verified false (the provider refuses the identity).
	cookie, state = s.start("google", `{}`, nil)
	s.google.errs["c4"] = oauth.ErrEmailUnverified
	if got := s.callback("google", q("c4", state), cookie, nil); got.location != "/login?error=email_unverified" {
		t.Fatalf("email unverified: %+v", got)
	}
	cookie, state = s.start("google", `{}`, nil)
	if got := s.callback("google", q("unknown-code", state), cookie, nil); got.location != "/login?error=failed" {
		t.Fatalf("failed exchange: %+v", got)
	}

	// Every outcome is audited.
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'auth.oauth_failed'`).Scan(&n); err != nil || n < 8 {
		t.Fatalf("oauth_failed audit rows: %d %v", n, err)
	}
}

func TestIntegrationOAuthHTTPInviteAndLink(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apioauthlink"))
	s := newOAuthServer(t, env, membersOpts(), api.Config{RegistrationEnabled: false, PasswordLoginEnabled: true})
	owner := adminSession(s.client)

	// Registration is off: a stranger is not invited.
	cookie, state := s.start("github", `{}`, nil)
	s.github.on("x", oauth.Identity{Provider: "github", Subject: "1", Email: "stranger@example.com"})
	if got := s.callback("github", q("x", state), cookie, nil); got.location != "/login?error=not_invited" {
		t.Fatalf("stranger: %+v", got)
	}

	// The invitation token travels in the start body only; a bad one is refused there.
	if r := s.do(http.MethodPost, "/api/v1/auth/oauth/github/start", `{"inviteToken":"nope"}`, nil); r.code != http.StatusNotFound ||
		r.body["type"] != "urn:maiak:problem:invitation-invalid" {
		t.Fatalf("bad invite: %d %s", r.code, r.raw)
	}
	token, _ := invite(t, s.client, owner, "member")
	b, _ := json.Marshal(map[string]string{"inviteToken": token})
	cookie, state = s.start("github", string(b), nil)
	if strings.Contains(cookie, token) {
		t.Fatal("the state cookie carries the invitation token")
	}
	s.github.on("inv", oauth.Identity{Provider: "github", Subject: "2", Email: "invited@example.com"})
	res := s.callback("github", q("inv", state), cookie, nil)
	if res.location != "/" || res.cookies[api.SessionCookie] == nil {
		t.Fatalf("invited sign-in: %+v", res)
	}
	invited := map[string]string{"Cookie": api.SessionCookie + "=" + res.cookies[api.SessionCookie].Value}
	if me := meOf(t, s.client, invited); me.body["role"] != "member" {
		t.Fatalf("invited role: %s", me.raw)
	}

	// Link: needs the session and the CSRF token.
	if r := s.do(http.MethodPost, "/api/v1/auth/oauth/google/start", `{"intent":"link"}`, nil); r.code != http.StatusUnauthorized {
		t.Fatalf("link without session: %d %s", r.code, r.raw)
	}
	if r := s.do(http.MethodPost, "/api/v1/auth/oauth/google/start", `{"intent":"link"}`, map[string]string{"Cookie": owner["Cookie"]}); r.code != http.StatusForbidden ||
		r.body["type"] != "urn:maiak:problem:csrf" {
		t.Fatalf("link without csrf: %d %s", r.code, r.raw)
	}
	if r := s.do(http.MethodPost, "/api/v1/auth/oauth/google/start", `{"intent":"link","inviteToken":"x"}`, owner); r.code != http.StatusBadRequest {
		t.Fatalf("link with invite: %d %s", r.code, r.raw)
	}
	cookie, state = s.start("google", `{"intent":"link"}`, owner)
	s.google.on("lk", oauth.Identity{Provider: "google", Subject: "g-admin", Email: "it@example.com", HostedDomain: "example.com"})
	// Another user's session (or none) at the callback cannot take over the link.
	if got := s.callback("google", q("lk", state), cookie, map[string]string{"Cookie": invited["Cookie"]}); got.location != "/profile?error=session_expired" {
		t.Fatalf("link with another session: %+v", got)
	}
	cookie, state = s.start("google", `{"intent":"link"}`, owner)
	if got := s.callback("google", q("lk", state), cookie, map[string]string{"Cookie": owner["Cookie"]}); got.location != "/profile?linked=google" ||
		got.cookies[api.SessionCookie] != nil {
		t.Fatalf("link: %+v", got)
	}
	ids := s.do(http.MethodGet, "/api/v1/auth/identities", "", owner)
	if ids.code != http.StatusOK || !strings.Contains(ids.raw, `"provider":"google"`) || ids.body["passwordSet"] != true {
		t.Fatalf("identities: %d %s", ids.code, ids.raw)
	}

	// The owner now proves the domain and claims it; a member is refused by role.
	r := s.do(http.MethodPost, "/api/v1/organization/auth-domains", `{"domain":"example.com","defaultRole":"member"}`, owner)
	if r.code != http.StatusCreated || r.body["domain"] != "example.com" {
		t.Fatalf("claim: %d %s", r.code, r.raw)
	}
	memberHdr := withCSRF(invited, meOf(t, s.client, invited).body["csrf_token"].(string))
	if r := s.do(http.MethodGet, "/api/v1/organization/auth-domains", "", memberHdr); r.code != http.StatusForbidden {
		t.Fatalf("member lists domains: %d %s", r.code, r.raw)
	}
	cookie, state = s.start("google", `{}`, nil)
	s.google.on("dom", oauth.Identity{Provider: "google", Subject: "g-colleague", Email: "colleague@example.com", HostedDomain: "example.com"})
	if got := s.callback("google", q("dom", state), cookie, nil); got.location != "/" {
		t.Fatalf("domain join: %+v", got)
	}

	// Unlink: the admin keeps its password, so the identity may go; then it is gone.
	if r := s.do(http.MethodDelete, "/api/v1/auth/identities/google", "", owner); r.code != http.StatusNoContent {
		t.Fatalf("unlink: %d %s", r.code, r.raw)
	}
	if r := s.do(http.MethodDelete, "/api/v1/auth/identities/google", "", owner); r.code != http.StatusNotFound {
		t.Fatalf("unlink again: %d %s", r.code, r.raw)
	}
	// The invited GitHub user has nothing else.
	if r := s.do(http.MethodDelete, "/api/v1/auth/identities/github", "", memberHdr); r.code != http.StatusConflict ||
		r.body["type"] != "urn:maiak:problem:last-login-method" {
		t.Fatalf("unlink the last method: %d %s", r.code, r.raw)
	}
}

func TestIntegrationOAuthHTTPRateLimitAndPasswordOff(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apioauthlimit"))
	opts := membersOpts()
	opts.IPLimiter = auth.NewLimiter(2, time.Minute, 100, clock.System{})
	opts.PasswordLoginDisabled = true
	s := newOAuthServer(t, env, opts, api.Config{PasswordLoginEnabled: false})

	if r := s.do(http.MethodGet, "/api/v1/auth/providers", "", nil); r.body["passwordLoginEnabled"] != false {
		t.Fatalf("providers: %s", r.raw)
	}
	r := s.do(http.MethodPost, "/api/v1/auth/login", `{"login":"someone@example.com","password":"x"}`, map[string]string{"X-Forwarded-For": "198.51.100.40"})
	if r.code != http.StatusForbidden || r.body["type"] != "urn:maiak:problem:password-login-disabled" {
		t.Fatalf("password login off: %d %s", r.code, r.raw)
	}

	// Failed callbacks share the login's per-IP bucket.
	hdr := map[string]string{"X-Forwarded-For": "198.51.100.41"}
	for i := 0; i < 2; i++ {
		if got := s.callback("google", q("c", "s"), "", hdr); got.location != "/login?error=state_invalid" {
			t.Fatalf("attempt %d: %+v", i, got)
		}
	}
	if got := s.callback("google", q("c", "s"), "", hdr); got.location != "/login?error=rate_limited" {
		t.Fatalf("third attempt: %+v", got)
	}
	r = s.do(http.MethodPost, "/api/v1/auth/login", `{"login":"admin","password":"`+adminPassword+`"}`, hdr)
	if r.code != http.StatusTooManyRequests {
		t.Fatalf("admin login from the same address: %d %s", r.code, r.raw)
	}
	// The break-glass admin signs in with the password from another address.
	if r := s.do(http.MethodPost, "/api/v1/auth/login", `{"login":"admin","password":"`+adminPassword+`"}`, map[string]string{"X-Forwarded-For": "198.51.100.42"}); r.code != http.StatusOK {
		t.Fatalf("admin password login: %d %s", r.code, r.raw)
	}
}
