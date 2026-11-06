package oauth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/oauth"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

const (
	clientID     = "client-id.apps.example"
	clientSecret = "client-secret-not-real"
	publicURL    = "https://maiak.example.com"
)

var stateKey = []byte("0123456789abcdef0123456789abcdef")

// fakeProvider is one TLS server that plays Google (discovery, token, keys) and GitHub (token,
// user, emails). Tests set the claims or the GitHub answers and inspect the token requests.
type fakeProvider struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	mu        sync.Mutex
	claims    map[string]any
	alg       string
	tokenForm url.Values
	ghUser    string
	ghEmails  string
	ghToken   string
	redirect  string // when set, the token endpoint redirects there
	bigBody   bool
	keyFetch  int
	discFetch int
	discFail  bool
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{t: t, key: key, kid: "k1", alg: "RS256", ghToken: `{"access_token":"gho_test","token_type":"bearer"}`,
		ghUser: `{"id":4242,"login":"octo","name":"Octo Cat"}`, ghEmails: `[{"email":"Octo@Example.com","primary":true,"verified":true}]`}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.discFetch++
		fail := f.discFail
		f.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		base := f.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": base, "authorization_endpoint": base + "/auth", "token_endpoint": base + "/token", "jwks_uri": base + "/keys",
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.keyFetch++
		f.mu.Unlock()
		e := big.NewInt(int64(f.key.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": f.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokenForm = r.PostForm
		redirect, big := f.redirect, f.bigBody
		f.mu.Unlock()
		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}
		if big {
			_, _ = w.Write([]byte(`{"id_token":"` + strings.Repeat("a", 2<<20) + `"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": f.idToken(), "access_token": "ya29.not-used"})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokenForm = r.PostForm
		f.mu.Unlock()
		_, _ = w.Write([]byte(f.ghToken))
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_test" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(f.ghUser))
	})
	mux.HandleFunc("GET /user/emails", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(f.ghEmails))
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProvider) setClaims(c map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = c
}

func (f *fakeProvider) idToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, _ := json.Marshal(map[string]string{"alg": f.alg, "kid": f.kid, "typ": "JWT"})
	c, _ := json.Marshal(f.claims)
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *fakeProvider) google(c clock.Clock) *oauth.Google {
	return oauth.NewGoogle(oauth.GoogleConfig{ClientID: clientID, ClientSecret: clientSecret, Issuer: f.srv.URL, HTTPClient: f.srv.Client(), Clock: c})
}

func (f *fakeProvider) github() *oauth.GitHub {
	return oauth.NewGitHub(oauth.GitHubConfig{ClientID: clientID, ClientSecret: clientSecret, WebURL: f.srv.URL, APIURL: f.srv.URL, HTTPClient: f.srv.Client()})
}

// start runs Flow.Start and returns the state cookie and the query of the authorization URL.
func start(t *testing.T, flow *oauth.Flow, provider string, intent oauth.Intent, user uuid.UUID, invite []byte) (string, url.Values) {
	t.Helper()
	raw, cookie, err := flow.Start(context.Background(), provider, intent, user, invite)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cookie, u.Query()
}

func googleClaims(now time.Time, nonce string) map[string]any {
	return map[string]any{
		"iss": "", "sub": "1098765", "aud": clientID, "azp": clientID, "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
		"nonce": nonce, "email": "Ada@Example.com", "email_verified": true, "hd": "Example.com", "name": "Ada",
	}
}

func TestGoogleFlow(t *testing.T) {
	f := newFakeProvider(t)
	now := time.Now().UTC()
	clk := clock.NewFake(now)
	flow, err := oauth.NewFlow(stateKey, publicURL, clk, f.google(clk))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cookie, q := start(t, flow, "google", oauth.IntentLogin, uuid.Nil, []byte{1, 2, 3})

	if q.Get("redirect_uri") != publicURL+"/api/v1/auth/oauth/google/callback" || q.Get("code_challenge_method") != "S256" ||
		q.Get("client_id") != clientID || q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		t.Fatalf("authorization URL query: %v", q)
	}
	if strings.Contains(cookie, clientSecret) {
		t.Fatal("cookie carries the client secret")
	}
	st, err := flow.ParseState("google", cookie, q.Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	if string(st.InviteHash) != "\x01\x02\x03" || st.Intent != oauth.IntentLogin {
		t.Fatalf("state: %+v", st)
	}
	// The challenge is S256 of the verifier kept in the cookie.
	sum := sha256.Sum256([]byte(st.Verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("code_challenge is not S256(verifier)")
	}

	claims := googleClaims(now, q.Get("nonce"))
	claims["iss"] = f.srv.URL
	f.setClaims(claims)
	id, err := flow.Exchange(ctx, st, "the-code")
	if err != nil {
		t.Fatal(err)
	}
	want := oauth.Identity{Provider: "google", Subject: "1098765", Email: "ada@example.com", HostedDomain: "example.com", DisplayName: "Ada"}
	if id != want {
		t.Fatalf("identity = %+v, want %+v", id, want)
	}
	f.mu.Lock()
	form := f.tokenForm
	f.mu.Unlock()
	if form.Get("code_verifier") != st.Verifier || form.Get("code") != "the-code" || form.Get("redirect_uri") != q.Get("redirect_uri") ||
		form.Get("client_secret") != clientSecret {
		t.Fatalf("token request: %v", form)
	}

	// ID token checks: each tampering is refused.
	cases := []struct {
		name string
		mut  func(map[string]any)
		want error
	}{
		{"email not verified", func(c map[string]any) { c["email_verified"] = false }, oauth.ErrEmailUnverified},
		{"email verified as string", func(c map[string]any) { c["email_verified"] = "true" }, nil},
		{"wrong nonce", func(c map[string]any) { c["nonce"] = "other" }, oauth.ErrProvider},
		{"wrong audience", func(c map[string]any) { c["aud"] = "someone-else" }, oauth.ErrProvider},
		{"several audiences without azp", func(c map[string]any) { c["aud"] = []string{clientID, "x"}; delete(c, "azp") }, oauth.ErrProvider},
		{"wrong azp", func(c map[string]any) { c["azp"] = "x" }, oauth.ErrProvider},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example" }, oauth.ErrProvider},
		{"expired", func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() }, oauth.ErrProvider},
		{"no subject", func(c map[string]any) { c["sub"] = "" }, oauth.ErrProvider},
		{"no hd is fine", func(c map[string]any) { delete(c, "hd") }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := googleClaims(now, st.Nonce)
			cl["iss"] = f.srv.URL
			c.mut(cl)
			f.setClaims(cl)
			_, err := flow.Exchange(ctx, st, "code")
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}

	t.Run("alg none refused", func(t *testing.T) {
		cl := googleClaims(now, st.Nonce)
		cl["iss"] = f.srv.URL
		f.setClaims(cl)
		f.mu.Lock()
		f.alg = "none"
		f.mu.Unlock()
		defer func() { f.mu.Lock(); f.alg = "RS256"; f.mu.Unlock() }()
		if _, err := flow.Exchange(ctx, st, "code"); !errors.Is(err, oauth.ErrProvider) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("redirect to another host refused", func(t *testing.T) {
		f.mu.Lock()
		f.redirect = "https://other.example/token"
		f.mu.Unlock()
		defer func() { f.mu.Lock(); f.redirect = ""; f.mu.Unlock() }()
		if _, err := flow.Exchange(ctx, st, "code"); !errors.Is(err, oauth.ErrProvider) || !strings.Contains(err.Error(), "another host") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("oversized answer refused", func(t *testing.T) {
		f.mu.Lock()
		f.bigBody = true
		f.mu.Unlock()
		defer func() { f.mu.Lock(); f.bigBody = false; f.mu.Unlock() }()
		if _, err := flow.Exchange(ctx, st, "code"); !errors.Is(err, oauth.ErrProvider) || !strings.Contains(err.Error(), "more than") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestGoogleRejectsForeignSignature(t *testing.T) {
	f := newFakeProvider(t)
	now := time.Now().UTC()
	clk := clock.NewFake(now)
	flow, _ := oauth.NewFlow(stateKey, publicURL, clk, f.google(clk))
	cookie, q := start(t, flow, "google", oauth.IntentLogin, uuid.Nil, nil)
	st, err := flow.ParseState("google", cookie, q.Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	cl := googleClaims(now, st.Nonce)
	cl["iss"] = f.srv.URL
	f.setClaims(cl)
	// Fetch the real keys once, then sign with a different key under the same kid.
	if _, err := flow.Exchange(context.Background(), st, "code"); err != nil {
		t.Fatal(err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.mu.Lock()
	f.key = other
	f.mu.Unlock()
	if _, err := flow.Exchange(context.Background(), st, "code"); !errors.Is(err, oauth.ErrProvider) || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v", err)
	}
}

func TestStateTampering(t *testing.T) {
	f := newFakeProvider(t)
	now := time.Now().UTC()
	clk := clock.NewFake(now)
	flow, _ := oauth.NewFlow(stateKey, publicURL, clk, f.google(clk), f.github())
	user := uuid.New()
	cookie, q := start(t, flow, "github", oauth.IntentLink, user, nil)
	state := q.Get("state")

	if st, err := flow.ParseState("github", cookie, state); err != nil || st.UserID != user || st.Intent != oauth.IntentLink {
		t.Fatalf("valid state: %+v %v", st, err)
	}
	payload, mac, _ := strings.Cut(cookie, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	forged := strings.Replace(string(raw), `"i":"link"`, `"i":"login"`, 1)
	other, _ := oauth.NewFlow([]byte("another key, 32 bytes long......"), publicURL, clk, f.github())
	otherCookie, otherQ := start(t, other, "github", oauth.IntentLogin, uuid.Nil, nil)

	cases := map[string]struct{ provider, cookie, state string }{
		"no cookie":             {"github", "", state},
		"no dot":                {"github", payload, state},
		"wrong state":           {"github", cookie, state + "x"},
		"empty state":           {"github", cookie, ""},
		"other provider":        {"google", cookie, state},
		"payload changed":       {"github", base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + mac, state},
		"mac truncated":         {"github", payload + "." + mac[:10], state},
		"signed with other key": {"github", otherCookie, otherQ.Get("state")},
	}
	for name, c := range cases {
		if _, err := flow.ParseState(c.provider, c.cookie, c.state); !errors.Is(err, oauth.ErrStateInvalid) {
			t.Errorf("%s: err = %v, want ErrStateInvalid", name, err)
		}
	}

	clk.Advance(oauth.StateTTL)
	if _, err := flow.ParseState("github", cookie, state); !errors.Is(err, oauth.ErrStateInvalid) {
		t.Fatalf("expired state accepted: %v", err)
	}
}

func TestGitHubFlow(t *testing.T) {
	f := newFakeProvider(t)
	flow, _ := oauth.NewFlow(stateKey, publicURL, nil, f.github())
	ctx := context.Background()
	cookie, q := start(t, flow, "github", oauth.IntentLogin, uuid.Nil, nil)
	if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != publicURL+"/api/v1/auth/oauth/github/callback" {
		t.Fatalf("authorization URL: %v", q)
	}
	st, err := flow.ParseState("github", cookie, q.Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := flow.Exchange(ctx, st, "code")
	if err != nil {
		t.Fatal(err)
	}
	if id != (oauth.Identity{Provider: "github", Subject: "4242", Email: "octo@example.com", DisplayName: "Octo Cat"}) {
		t.Fatalf("identity: %+v", id)
	}
	f.mu.Lock()
	verifier := f.tokenForm.Get("code_verifier")
	f.mu.Unlock()
	if verifier != st.Verifier {
		t.Fatal("PKCE verifier not sent")
	}

	set := func(field *string, v string) func() {
		f.mu.Lock()
		old := *field
		*field = v
		f.mu.Unlock()
		return func() { f.mu.Lock(); *field = old; f.mu.Unlock() }
	}
	cases := []struct {
		name  string
		field *string
		value string
		want  error
	}{
		{"primary not verified", &f.ghEmails, `[{"email":"a@example.com","primary":true,"verified":false},{"email":"b@example.com","primary":false,"verified":true}]`, oauth.ErrEmailUnverified},
		{"no emails", &f.ghEmails, `[]`, oauth.ErrEmailUnverified},
		{"token error", &f.ghToken, `{"error":"bad_verification_code","error_description":"The code passed is incorrect"}`, oauth.ErrProvider},
		{"user without id", &f.ghUser, `{"login":"x"}`, oauth.ErrProvider},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer set(c.field, c.value)()
			_, err := flow.Exchange(ctx, st, "code")
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if err != nil && strings.Contains(err.Error(), "incorrect") {
				t.Fatal("provider error text leaks into the error")
			}
		})
	}
}

func TestNewHTTPClient(t *testing.T) {
	c := oauth.NewHTTPClient()
	if c.Timeout <= 0 || c.CheckRedirect == nil {
		t.Fatal("client without timeout or redirect policy")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("client uses a proxy from the environment")
	}
}

// A failed discovery refresh serves the stale document and is not retried for a while (O-04 L1).
func TestGoogleDiscoveryBackoff(t *testing.T) {
	f := newFakeProvider(t)
	clk := clock.NewFake(time.Now())
	g := f.google(clk)
	ctx := context.Background()
	authURL := func() error {
		_, err := g.AuthCodeURL(ctx, oauth.AuthRequest{State: "s", Nonce: "n", CodeChallenge: "c", RedirectURI: publicURL})
		return err
	}
	fetches := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.discFetch }
	if err := authURL(); err != nil || fetches() != 1 {
		t.Fatalf("first: %v, fetches %d", err, fetches())
	}
	f.mu.Lock()
	f.discFail = true
	f.mu.Unlock()
	clk.Advance(25 * time.Hour)
	for i := 0; i < 3; i++ {
		if err := authURL(); err != nil {
			t.Fatalf("stale copy not served: %v", err)
		}
	}
	if fetches() != 2 {
		t.Fatalf("fetches during backoff = %d, want 2", fetches())
	}
	clk.Advance(31 * time.Second)
	if err := authURL(); err != nil || fetches() != 3 {
		t.Fatalf("after backoff: %v, fetches %d", err, fetches())
	}
	// Without any copy the failure is returned.
	cold := f.google(clk)
	if _, err := cold.AuthCodeURL(ctx, oauth.AuthRequest{}); !errors.Is(err, oauth.ErrProvider) {
		t.Fatalf("cold failure: %v", err)
	}
}

// Concurrent sign-ins share one discovery fetch.
func TestGoogleDiscoverySingleFetch(t *testing.T) {
	f := newFakeProvider(t)
	g := f.google(nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.AuthCodeURL(context.Background(), oauth.AuthRequest{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.discFetch > 2 {
		t.Fatalf("discovery fetched %d times for one cold start", f.discFetch)
	}
}
