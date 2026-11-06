package oauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
)

// GoogleIssuer is Google's OpenID Connect issuer.
const GoogleIssuer = "https://accounts.google.com"

const (
	discoveryTTL = 24 * time.Hour
	// jwksRefreshMin limits refetching the keys for an unknown kid (key rotation) to once a minute.
	jwksRefreshMin = time.Minute
	// clockSkew is the tolerance for exp and iat.
	clockSkew = 2 * time.Minute
	maxJWKs   = 20
)

// GoogleConfig configures the Google provider.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	// Issuer defaults to GoogleIssuer; tests point it at a fake server.
	Issuer string
	// HTTPClient defaults to NewHTTPClient(); its redirect policy and timeout are enforced anyway.
	HTTPClient *http.Client
	Clock      clock.Clock
}

// Google is Google sign-in through OpenID Connect: discovery, the authorization code flow with
// PKCE and nonce, and a validated ID token (RS256 against the published keys, iss, aud, azp, exp,
// nonce). Only accounts with email_verified are accepted; hd is the Workspace domain.
type Google struct {
	cfg    GoogleConfig
	client *http.Client

	mu        sync.Mutex
	disc      *discovery
	discAt    time.Time
	keys      map[string]*rsa.PublicKey
	keysAt    time.Time
	keysFetch time.Time
}

type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// NewGoogle builds the provider.
func NewGoogle(cfg GoogleConfig) *Google {
	if cfg.Issuer == "" {
		cfg.Issuer = GoogleIssuer
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	return &Google{cfg: cfg, client: hardened(cfg.HTTPClient)}
}

// Name implements Provider.
func (g *Google) Name() string { return "google" }

// DisplayName implements Provider.
func (g *Google) DisplayName() string { return "Google" }

// AuthCodeURL implements Provider.
func (g *Google) AuthCodeURL(ctx context.Context, r AuthRequest) (string, error) {
	d, err := g.discovery(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {g.cfg.ClientID},
		"redirect_uri":          {r.RedirectURI},
		"scope":                 {"openid email profile"},
		"state":                 {r.State},
		"nonce":                 {r.Nonce},
		"code_challenge":        {r.CodeChallenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return d.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// Exchange implements Provider.
func (g *Google) Exchange(ctx context.Context, r TokenRequest) (Identity, error) {
	d, err := g.discovery(ctx)
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {r.Code},
		"redirect_uri":  {r.RedirectURI},
		"client_id":     {g.cfg.ClientID},
		"client_secret": {g.cfg.ClientSecret},
		"code_verifier": {r.CodeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrProvider, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := doJSON(g.client, req, &tok); err != nil {
		return Identity{}, err
	}
	claims, err := g.verifyIDToken(ctx, d, tok.IDToken, r.Nonce)
	if err != nil {
		return Identity{}, err
	}
	if !claims.EmailVerified {
		return Identity{}, ErrEmailUnverified
	}
	return Identity{
		Subject:      claims.Sub,
		Email:        strings.ToLower(claims.Email),
		HostedDomain: strings.ToLower(claims.HD),
		DisplayName:  truncate(claims.Name, 200),
	}, nil
}

type googleClaims struct {
	Iss           string       `json:"iss"`
	Sub           string       `json:"sub"`
	Aud           audience     `json:"aud"`
	Azp           string       `json:"azp"`
	Exp           int64        `json:"exp"`
	Iat           int64        `json:"iat"`
	Nonce         string       `json:"nonce"`
	Email         string       `json:"email"`
	EmailVerified flexibleBool `json:"email_verified"`
	HD            string       `json:"hd"`
	Name          string       `json:"name"`
}

// audience is the aud claim: a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// flexibleBool accepts true and "true" (older Google tokens sent the string).
type flexibleBool bool

func (f *flexibleBool) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case "true", `"true"`:
		*f = true
	default:
		*f = false
	}
	return nil
}

// verifyIDToken checks the JWS signature (RS256 only) and the claims of an ID token.
func (g *Google) verifyIDToken(ctx context.Context, d *discovery, token, nonce string) (googleClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return googleClaims{}, fmt.Errorf("%w: id token is not a JWS", ErrProvider)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return googleClaims{}, err
	}
	if header.Alg != "RS256" {
		return googleClaims{}, fmt.Errorf("%w: id token alg %q not accepted", ErrProvider, header.Alg)
	}
	key, err := g.key(ctx, d, header.Kid)
	if err != nil {
		return googleClaims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return googleClaims{}, fmt.Errorf("%w: id token signature encoding", ErrProvider)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return googleClaims{}, fmt.Errorf("%w: id token signature", ErrProvider)
	}
	var c googleClaims
	if err := decodeSegment(parts[1], &c); err != nil {
		return googleClaims{}, err
	}
	now := g.cfg.Clock.Now()
	issOK := c.Iss == d.Issuer || (d.Issuer == GoogleIssuer && c.Iss == "accounts.google.com")
	audOK := false
	for _, a := range c.Aud {
		audOK = audOK || a == g.cfg.ClientID
	}
	switch {
	case !issOK:
		return googleClaims{}, fmt.Errorf("%w: id token issuer", ErrProvider)
	case !audOK, len(c.Aud) > 1 && c.Azp != g.cfg.ClientID, c.Azp != "" && c.Azp != g.cfg.ClientID:
		return googleClaims{}, fmt.Errorf("%w: id token audience", ErrProvider)
	case c.Exp == 0 || now.After(time.Unix(c.Exp, 0).Add(clockSkew)):
		return googleClaims{}, fmt.Errorf("%w: id token expired", ErrProvider)
	case c.Iat != 0 && time.Unix(c.Iat, 0).After(now.Add(clockSkew)):
		return googleClaims{}, fmt.Errorf("%w: id token issued in the future", ErrProvider)
	case nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1:
		return googleClaims{}, fmt.Errorf("%w: id token nonce", ErrProvider)
	case c.Sub == "" || len(c.Sub) > 255:
		return googleClaims{}, fmt.Errorf("%w: id token subject", ErrProvider)
	case c.Email == "":
		return googleClaims{}, ErrEmailUnverified
	}
	return c, nil
}

func decodeSegment(seg string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return fmt.Errorf("%w: id token encoding", ErrProvider)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: id token json", ErrProvider)
	}
	return nil
}

func (g *Google) discovery(ctx context.Context) (*discovery, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.cfg.Clock.Now()
	if g.disc != nil && now.Sub(g.discAt) < discoveryTTL {
		return g.disc, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.Issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProvider, err)
	}
	var d discovery
	if err := doJSON(g.client, req, &d); err != nil {
		if g.disc != nil {
			// A stale document beats no sign-in while Google's discovery endpoint hiccups.
			return g.disc, nil
		}
		return nil, err
	}
	if d.Issuer != g.cfg.Issuer || !isHTTPS(d.AuthorizationEndpoint) || !isHTTPS(d.TokenEndpoint) || !isHTTPS(d.JWKSURI) {
		return nil, fmt.Errorf("%w: discovery document has an unexpected issuer or non-https endpoints", ErrProvider)
	}
	g.disc, g.discAt = &d, now
	return g.disc, nil
}

func isHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// key returns the RSA key with that kid, refetching the key set when it is unknown or older than
// the discovery TTL (at most once per jwksRefreshMin).
func (g *Google) key(ctx context.Context, d *discovery, kid string) (*rsa.PublicKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.cfg.Clock.Now()
	if k, ok := g.keys[kid]; ok && now.Sub(g.keysAt) < discoveryTTL {
		return k, nil
	}
	if now.Sub(g.keysFetch) < jwksRefreshMin {
		if k, ok := g.keys[kid]; ok {
			return k, nil
		}
		return nil, fmt.Errorf("%w: unknown id token key", ErrProvider)
	}
	g.keysFetch = now
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.JWKSURI, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProvider, err)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := doJSON(g.client, req, &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for i, k := range set.Keys {
		if i >= maxJWKs {
			break
		}
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pk, err := rsaKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pk
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no usable keys in the key set", ErrProvider)
	}
	g.keys, g.keysAt = keys, now
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown id token key", ErrProvider)
}

func rsaKey(n64, e64 string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n64)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(e64)
	if err != nil || len(eb) == 0 || len(eb) > 4 {
		return nil, errors.New("bad exponent")
	}
	e := 0
	for _, b := range eb {
		e = e<<8 | int(b)
	}
	n := new(big.Int).SetBytes(nb)
	if n.BitLen() < 2048 || e < 3 {
		return nil, errors.New("weak key")
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}
