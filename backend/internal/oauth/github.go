package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// GitHub endpoints.
const (
	GitHubWebURL = "https://github.com"
	GitHubAPIURL = "https://api.github.com"
)

// GitHubConfig configures the GitHub provider.
type GitHubConfig struct {
	ClientID     string
	ClientSecret string
	// WebURL and APIURL default to github.com; tests point them at a fake server.
	WebURL     string
	APIURL     string
	HTTPClient *http.Client
}

// GitHub is sign-in with a GitHub OAuth app (authorization code with PKCE). The stable key is the
// numeric user id; the email is the primary address, used only when GitHub marks it verified.
type GitHub struct {
	cfg    GitHubConfig
	client *http.Client
}

// NewGitHub builds the provider.
func NewGitHub(cfg GitHubConfig) *GitHub {
	if cfg.WebURL == "" {
		cfg.WebURL = GitHubWebURL
	}
	if cfg.APIURL == "" {
		cfg.APIURL = GitHubAPIURL
	}
	return &GitHub{cfg: cfg, client: hardened(cfg.HTTPClient)}
}

// Name implements Provider.
func (g *GitHub) Name() string { return "github" }

// DisplayName implements Provider.
func (g *GitHub) DisplayName() string { return "GitHub" }

// AuthCodeURL implements Provider.
func (g *GitHub) AuthCodeURL(_ context.Context, r AuthRequest) (string, error) {
	q := url.Values{
		"client_id":             {g.cfg.ClientID},
		"redirect_uri":          {r.RedirectURI},
		"scope":                 {"read:user user:email"},
		"state":                 {r.State},
		"code_challenge":        {r.CodeChallenge},
		"code_challenge_method": {"S256"},
		"allow_signup":          {"false"},
	}
	return g.cfg.WebURL + "/login/oauth/authorize?" + q.Encode(), nil
}

// Exchange implements Provider. The access token is used for the two API calls and dropped.
func (g *GitHub) Exchange(ctx context.Context, r TokenRequest) (Identity, error) {
	form := url.Values{
		"client_id":     {g.cfg.ClientID},
		"client_secret": {g.cfg.ClientSecret},
		"code":          {r.Code},
		"redirect_uri":  {r.RedirectURI},
		"code_verifier": {r.CodeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.WebURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrProvider, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := doJSON(g.client, req, &tok); err != nil {
		return Identity{}, err
	}
	if tok.Error != "" || tok.AccessToken == "" {
		// GitHub answers 200 with an error code for a bad or expired code; the code is logged,
		// never its description.
		return Identity{}, fmt.Errorf("%w: token endpoint error %q", ErrProvider, truncate(tok.Error, 64))
	}

	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := g.api(ctx, tok.AccessToken, "/user", &user); err != nil {
		return Identity{}, err
	}
	if user.ID <= 0 {
		return Identity{}, fmt.Errorf("%w: user without id", ErrProvider)
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := g.api(ctx, tok.AccessToken, "/user/emails", &emails); err != nil {
		return Identity{}, err
	}
	var email string
	for _, e := range emails {
		if e.Primary && e.Verified {
			email = strings.ToLower(e.Email)
		}
	}
	if email == "" {
		return Identity{}, ErrEmailUnverified
	}
	name := user.Name
	if name == "" {
		name = user.Login
	}
	return Identity{Subject: strconv.FormatInt(user.ID, 10), Email: email, DisplayName: truncate(name, 200)}, nil
}

func (g *GitHub) api(ctx context.Context, token, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.APIURL+path, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrProvider, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return doJSON(g.client, req, v)
}
