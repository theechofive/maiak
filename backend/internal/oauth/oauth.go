// Package oauth runs the OAuth 2.0 authorization code flow with PKCE for sign-in with an external
// identity provider (ADR-0104). It owns the Provider interface (Google OpenID Connect and GitHub
// OAuth implement it), the signed state cookie that binds a flow to the browser that started it,
// and the hardened HTTP client the providers use. It never stores provider tokens: a flow ends
// with a verified Identity, and internal/auth decides what it may do.
package oauth

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

// StateTTL is how long a started flow may take until the callback.
const StateTTL = 10 * time.Minute

// CallbackPath is the path of the redirect URI under MAIAK_PUBLIC_URL; %s is the provider.
const CallbackPath = "/api/v1/auth/oauth/%s/callback"

// Intent is what a finished flow is for.
type Intent string

// Intents.
const (
	IntentLogin Intent = "login"
	IntentLink  Intent = "link"
)

var (
	// ErrStateInvalid means the callback does not belong to a flow this browser started: no
	// cookie, a forged or expired one, another provider, or a state that does not match.
	ErrStateInvalid = errors.New("oauth: state invalid")
	// ErrEmailUnverified means the provider has no verified email for the account (Google
	// email_verified false, no primary verified GitHub address).
	ErrEmailUnverified = errors.New("oauth: email not verified")
	// ErrProvider means the provider refused the code or answered something unusable. The cause is
	// wrapped for the logs only; it is never shown to the user.
	ErrProvider = errors.New("oauth: provider exchange failed")
)

// Identity is a verified account at a provider.
type Identity struct {
	Provider string
	// Subject is the stable id at the provider (Google sub, GitHub numeric user id).
	Subject string
	// Email is the verified address, lowercased.
	Email string
	// HostedDomain is Google's hd claim (the Workspace domain), lowercased; empty otherwise.
	HostedDomain string
	// DisplayName is the account's name, if the provider gives one.
	DisplayName string
}

// AuthRequest is what a provider needs to build its authorization URL.
type AuthRequest struct {
	State, Nonce, CodeChallenge, RedirectURI string
}

// TokenRequest is what a provider needs to exchange the code.
type TokenRequest struct {
	Code, CodeVerifier, Nonce, RedirectURI string
}

// Provider is an identity provider. Implementations validate everything they return: an Identity
// always has a subject and a verified email.
type Provider interface {
	// Name is the id used in paths and in auth_identities.provider (google, github).
	Name() string
	// DisplayName is the label of the login button.
	DisplayName() string
	AuthCodeURL(ctx context.Context, r AuthRequest) (string, error)
	Exchange(ctx context.Context, r TokenRequest) (Identity, error)
}

// State is what the signed cookie carries between start and callback.
type State struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Intent   Intent `json:"i"`
	// UserID is the signed-in user of a link flow; the callback links only to that user.
	UserID uuid.UUID `json:"u,omitzero"`
	// InviteHash is the SHA-256 of the invitation token of a login flow; the token itself never
	// leaves the start request's body.
	InviteHash []byte `json:"h,omitempty"`
	Expires    int64  `json:"e"`
}

// Flow starts and finishes authorization code flows for the configured providers.
type Flow struct {
	key       []byte
	publicURL string
	clock     clock.Clock
	providers []Provider
}

// DeriveKey derives the state cookie's HMAC key from a 32-byte secret (the active encryption key),
// so the replicas share it without one more secret.
func DeriveKey(secret []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, nil, "maiak oauth state v1", 32)
}

// NewFlow builds a flow. publicURL is MAIAK_PUBLIC_URL; providers are listed in the order the
// login page shows them. A nil clock is the system clock.
func NewFlow(key []byte, publicURL string, c clock.Clock, providers ...Provider) (*Flow, error) {
	if len(key) < 32 {
		return nil, errors.New("oauth: state key must have at least 32 bytes")
	}
	if c == nil {
		c = clock.System{}
	}
	return &Flow{key: key, publicURL: publicURL, clock: c, providers: providers}, nil
}

// Providers returns the configured providers in display order.
func (f *Flow) Providers() []Provider { return f.providers }

// Provider returns the configured provider with that name.
func (f *Flow) Provider(name string) (Provider, bool) {
	for _, p := range f.providers {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// RedirectURI is the exact callback URL registered at the provider.
func (f *Flow) RedirectURI(provider string) string {
	return f.publicURL + fmt.Sprintf(CallbackPath, provider)
}

// Start begins a flow: it returns the provider's authorization URL and the signed cookie value
// that the callback needs. userID is set for IntentLink, inviteHash optionally for IntentLogin.
func (f *Flow) Start(ctx context.Context, provider string, intent Intent, userID uuid.UUID, inviteHash []byte) (string, string, error) {
	p, ok := f.Provider(provider)
	if !ok {
		return "", "", fmt.Errorf("oauth: provider %q not configured", provider)
	}
	st := State{Provider: provider, Intent: intent, UserID: userID, InviteHash: inviteHash,
		State: randomString(), Nonce: randomString(), Verifier: randomString(),
		Expires: f.clock.Now().Add(StateTTL).Unix()}
	u, err := p.AuthCodeURL(ctx, AuthRequest{
		State: st.State, Nonce: st.Nonce, CodeChallenge: challenge(st.Verifier), RedirectURI: f.RedirectURI(provider),
	})
	if err != nil {
		return "", "", err
	}
	cookie, err := f.sign(st)
	if err != nil {
		return "", "", err
	}
	return u, cookie, nil
}

// ParseState checks the cookie of a callback: signature, expiry, provider and the state
// parameter. Any mismatch is ErrStateInvalid.
func (f *Flow) ParseState(provider, cookie, state string) (State, error) {
	payload, mac, ok := strings.Cut(cookie, ".")
	if !ok {
		return State{}, ErrStateInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil || !hmac.Equal(got, f.mac(payload)) {
		return State{}, ErrStateInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return State{}, ErrStateInvalid
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, ErrStateInvalid
	}
	if st.Provider != provider || f.clock.Now().Unix() >= st.Expires || st.State == "" ||
		subtle.ConstantTimeCompare([]byte(st.State), []byte(state)) != 1 ||
		(st.Intent != IntentLogin && st.Intent != IntentLink) {
		return State{}, ErrStateInvalid
	}
	return st, nil
}

// Exchange finishes a flow whose state ParseState accepted: it trades the code with the PKCE
// verifier and returns the verified identity.
func (f *Flow) Exchange(ctx context.Context, st State, code string) (Identity, error) {
	p, ok := f.Provider(st.Provider)
	if !ok || code == "" {
		return Identity{}, ErrProvider
	}
	id, err := p.Exchange(ctx, TokenRequest{Code: code, CodeVerifier: st.Verifier, Nonce: st.Nonce, RedirectURI: f.RedirectURI(st.Provider)})
	if err != nil {
		return Identity{}, err
	}
	id.Provider = st.Provider
	return id, nil
}

func (f *Flow) sign(st State) (string, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("oauth: encode state: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(f.mac(payload)), nil
}

func (f *Flow) mac(payload string) []byte {
	h := hmac.New(sha256.New, f.key)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// randomString returns 32 random bytes, URL-safe base64: state, nonce and the PKCE verifier
// (43 characters, within RFC 7636's 43-128).
func randomString() string {
	b := make([]byte, 32)
	// crypto/rand.Read never fails (Go 1.24+); it panics if the system source is broken.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// challenge is the S256 code challenge of a PKCE verifier.
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// HTTP client limits for provider calls.
const (
	clientTimeout = 15 * time.Second
	maxBodyBytes  = 1 << 20
	maxRedirects  = 3
)

// NewHTTPClient returns the client for provider endpoints: overall and per-phase timeouts, no
// proxy from the environment, and redirects only within the same scheme and host.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: clientTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: sameHostRedirect,
	}
}

// hardened copies a client (tests pass one that trusts their TLS server) and enforces the
// redirect policy and a timeout whatever it had.
func hardened(c *http.Client) *http.Client {
	if c == nil {
		return NewHTTPClient()
	}
	cp := *c
	cp.CheckRedirect = sameHostRedirect
	if cp.Timeout <= 0 || cp.Timeout > clientTimeout {
		cp.Timeout = clientTimeout
	}
	return &cp
}

func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("oauth: too many redirects")
	}
	if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return errors.New("oauth: redirect to another host refused")
	}
	return nil
}

// doJSON sends req and decodes a 2xx JSON answer of at most maxBodyBytes into v. Error bodies are
// drained but never returned (they may echo secrets or provider text).
func doJSON(c *http.Client, req *http.Request, v any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrProvider, req.Method, req.URL.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read %s: %w", ErrProvider, req.URL.Host, err)
	}
	if len(body) > maxBodyBytes {
		return fmt.Errorf("%w: %s answered more than %d bytes", ErrProvider, req.URL.Host, maxBodyBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: %s %s answered HTTP %d", ErrProvider, req.Method, req.URL.Host+req.URL.Path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: decode %s: %w", ErrProvider, req.URL.Host+req.URL.Path, err)
	}
	return nil
}

// truncate cuts a display name to n bytes on a rune boundary.
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
