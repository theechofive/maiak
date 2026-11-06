package public

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/httpx"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/statuspages"
)

//go:embed assets/status.css assets/templates.html
var assets embed.FS

var (
	stylesheet = mustRead("assets/status.css")
	// cssVersion busts caches of the long-lived stylesheet when it changes.
	cssVersion = func() string { s := sha256.Sum256(stylesheet); return hex.EncodeToString(s[:6]) }()
	templates  = template.Must(template.New("").ParseFS(assets, "assets/templates.html"))
	slugRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,62}$`)
)

func mustRead(name string) []byte {
	b, err := assets.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

// Limits of one replica (L-274): page views per client IP, and password attempts per client IP and
// per page (the second bound slows a guess spread over many addresses).
const (
	viewRate        = 2.0 // per second on average
	viewBurst       = 60
	unlockPerIP     = 10 // per 15 minutes
	unlockPerPage   = 100
	unlockWindow    = 15 * time.Minute
	maxLimiterKeys  = 50_000
	maxPasswordForm = 4 << 10
)

// Config are the renderer's settings.
type Config struct {
	// PublicURL is MAIAK_PUBLIC_URL, the origin of every link the renderer writes (badge, embed).
	PublicURL string
	// TrustedProxyHops is MAIAK_TRUSTED_PROXY_HOPS, for the client IP of the limits.
	TrustedProxyHops int
	CookieSecure     bool
}

// Deps are the collaborators of the handler.
type Deps struct {
	Source  Source
	Cookies *CookieSigner
	Clock   clock.Clock
	Logger  *slog.Logger
	// Registerer receives the renderer's metrics; nil keeps them unregistered.
	Registerer prometheus.Registerer
	// HTTPMetrics instruments every request by route template; nil skips it.
	HTTPMetrics *metrics.HTTP
}

type handler struct {
	cfg      Config
	cache    *Cache
	cookies  *CookieSigner
	clock    clock.Clock
	log      *slog.Logger
	views    *Limiter
	unlockIP *Limiter
	unlockPg *Limiter
	// argon is a semaphore: one password verification at a time per replica (64 MiB each).
	argon chan struct{}
	m     *rendererMetrics
}

type rendererMetrics struct {
	cache   *prometheus.CounterVec
	limited *prometheus.CounterVec
	unlocks *prometheus.CounterVec
	errors  prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) *rendererMetrics {
	m := &rendererMetrics{
		cache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_statuspage_cache_lookups_total", Help: "Snapshot lookups by result (hit, miss).",
		}, []string{"result"}),
		limited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_statuspage_rate_limited_total", Help: "Requests refused by a rate limit, by kind (view, unlock).",
		}, []string{"kind"}),
		unlocks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_statuspage_unlock_attempts_total", Help: "Password attempts on protected pages, by result (ok, wrong).",
		}, []string{"result"}),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "maiak_statuspage_snapshot_errors_total", Help: "Snapshot lookups that failed (database or decoding).",
		}),
	}
	for _, r := range []string{"hit", "miss"} {
		m.cache.WithLabelValues(r)
	}
	for _, k := range []string{"view", "unlock"} {
		m.limited.WithLabelValues(k)
	}
	for _, r := range []string{"ok", "wrong"} {
		m.unlocks.WithLabelValues(r)
	}
	if reg != nil {
		reg.MustRegister(m.cache, m.limited, m.unlocks, m.errors)
	}
	return m
}

// NewHandler builds the public HTTP handler of cmd/statuspage.
func NewHandler(cfg Config, deps Deps) http.Handler {
	clk := deps.Clock
	if clk == nil {
		clk = clock.System{}
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &handler{cfg: cfg, cache: NewCache(deps.Source, clk), cookies: deps.Cookies, clock: clk, log: log,
		views:    NewLimiter(viewRate, viewBurst, maxLimiterKeys, clk),
		unlockIP: NewLimiter(float64(unlockPerIP)/unlockWindow.Seconds(), unlockPerIP, maxLimiterKeys, clk),
		unlockPg: NewLimiter(float64(unlockPerPage)/unlockWindow.Seconds(), unlockPerPage, maxLimiterKeys, clk),
		argon:    make(chan struct{}, 1),
		m:        newMetrics(deps.Registerer)}
	h.cache.OnLookup = func(hit bool) {
		if hit {
			h.m.cache.WithLabelValues("hit").Inc()
		} else {
			h.m.cache.WithLabelValues("miss").Inc()
		}
	}

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.AccessLog(log), httpx.Recover(log))
	if deps.HTTPMetrics != nil {
		r.Use(deps.HTTPMetrics.Middleware)
	}
	r.Use(middleware.GetHead, baseHeaders, httpx.MaxBody(maxPasswordForm), h.limitViews)
	r.Get("/status/_assets/status.css", h.serveCSS)
	r.Get("/status/{slug:[a-z0-9-]+}.json", h.serveJSON)
	r.Get("/status/{slug:[a-z0-9-]+}/feed.rss", h.serveRSS)
	r.Get("/status/{slug:[a-z0-9-]+}/badge", h.serveBadge)
	r.Get("/status/{slug:[a-z0-9-]+}/embed.js", h.serveEmbed)
	r.Post("/status/{slug:[a-z0-9-]+}/unlock", h.unlock)
	r.Get("/status/{slug:[a-z0-9-]+}", h.servePage)
	r.NotFound(h.notFound)
	r.MethodNotAllowed(h.notFound)
	return r
}

// baseHeaders apply to every answer, the 404 included.
func baseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		hd.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		hd.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (h *handler) clientKey(r *http.Request) string {
	ip, ok := httpx.ForwardedClientIP(r, h.cfg.TrustedProxyHops)
	if !ok || !ip.IsValid() {
		// Without a usable X-Forwarded-For every such request shares one bucket.
		return "unknown"
	}
	return ip.String()
}

func (h *handler) limitViews(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := h.views.Allow(h.clientKey(r)); !ok {
			h.m.limited.WithLabelValues("view").Inc()
			tooMany(w, wait)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte("Too many requests. Try again in a moment.\n"))
}

// lookup returns the entry of a well-formed slug; anything else is not found.
func (h *handler) lookup(r *http.Request) (string, Entry, bool) {
	slug := chi.URLParam(r, "slug")
	if !slugRe.MatchString(slug) {
		return slug, Entry{}, false
	}
	e, err := h.cache.Lookup(r.Context(), slug)
	if err != nil {
		h.m.errors.Inc()
		h.log.ErrorContext(r.Context(), "status page lookup failed", "error", err)
		return slug, Entry{}, false
	}
	return slug, e, e.Found
}

// notFound is the one answer for unknown, unpublished, deleted and suspended pages, protected
// pages on their JSON, RSS, badge and embed routes, and every other path: same status, headers and
// body, so none of these cases can be told apart.
func (h *handler) notFound(w http.ResponseWriter, _ *http.Request) {
	var buf bytes.Buffer
	_ = templates.ExecuteTemplate(&buf, "notfound", map[string]any{"CSSURL": cssURL()})
	hd := w.Header()
	hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("Cache-Control", "public, max-age=30")
	hd.Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(buf.Bytes())
}

func cssURL() string { return "/status/_assets/status.css?v=" + cssVersion }

func (h *handler) serveCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", `"`+cssVersion+`"`)
	if r.Header.Get("If-None-Match") == `"`+cssVersion+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(stylesheet)
}

func newNonce() string {
	// Hex: valid in a CSP nonce and never changed by the template's attribute escaping.
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pageHeaders is the policy of the HTML page: styles from this origin plus one nonce'd element,
// images only from the logo's origin, no scripts, no frames, forms only to this origin.
func pageHeaders(w http.ResponseWriter, nonce, imgSrc string, protected bool) {
	hd := w.Header()
	img := "'none'"
	if imgSrc != "" {
		img = imgSrc
	}
	hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'nonce-"+nonce+"'; img-src "+img+
		"; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	hd.Set("Content-Type", "text/html; charset=utf-8")
	if protected {
		hd.Set("Cache-Control", "private, no-store")
		hd.Set("X-Robots-Tag", "noindex, nofollow")
	} else {
		hd.Set("Cache-Control", "public, max-age=30")
	}
}

func (h *handler) servePage(w http.ResponseWriter, r *http.Request) {
	slug, e, ok := h.lookup(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	protected := e.PasswordHash != ""
	if protected && !h.unlocked(r, slug, e) {
		h.renderLock(w, r, slug, e, "", http.StatusOK)
		return
	}
	nonce := newNonce()
	v := h.pageView(slug, e.Snapshot, nonce)
	v.NoIndex = protected
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "page", v); err != nil {
		h.log.ErrorContext(r.Context(), "render status page", "error", err)
		h.notFound(w, r)
		return
	}
	pageHeaders(w, nonce, v.imgSrc, protected)
	if protected {
		w.Header().Set("Vary", "Cookie")
	}
	_, _ = w.Write(buf.Bytes())
}

func (h *handler) unlocked(r *http.Request, slug string, e Entry) bool {
	c, err := r.Cookie(CookieName)
	return err == nil && h.cookies != nil && h.cookies.Valid(c.Value, slug, e.PasswordHash, h.clock.Now())
}

func (h *handler) renderLock(w http.ResponseWriter, r *http.Request, slug string, e Entry, msg string, status int) {
	nonce := newNonce()
	v := h.pageView(slug, e.Snapshot, nonce)
	v.NoIndex, v.Error = true, msg
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "lock", v); err != nil {
		h.log.ErrorContext(r.Context(), "render lock page", "error", err)
		h.notFound(w, r)
		return
	}
	pageHeaders(w, nonce, "", true)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// unlock checks a password (argon2id, one at a time, limited per IP and per page) and sets the
// signed cookie scoped to the page's path.
func (h *handler) unlock(w http.ResponseWriter, r *http.Request) {
	slug, e, ok := h.lookup(r)
	if !ok || e.PasswordHash == "" {
		h.notFound(w, r)
		return
	}
	if !h.sameOrigin(r) {
		h.notFound(w, r)
		return
	}
	if ok, wait := h.unlockIP.Allow(h.clientKey(r)); !ok {
		h.m.limited.WithLabelValues("unlock").Inc()
		tooMany(w, wait)
		return
	}
	if ok, wait := h.unlockPg.Allow(slug); !ok {
		h.m.limited.WithLabelValues("unlock").Inc()
		tooMany(w, wait)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderLock(w, r, slug, e, "The password could not be read. Try again.", http.StatusBadRequest)
		return
	}
	password := r.PostForm.Get("password")
	if password == "" || len(password) > statuspages.MaxPasswordBytes || !h.verify(r.Context(), password, e.PasswordHash) {
		h.m.unlocks.WithLabelValues("wrong").Inc()
		h.renderLock(w, r, slug, e, "That password is not correct.", http.StatusUnauthorized)
		return
	}
	h.m.unlocks.WithLabelValues("ok").Inc()
	// Secure comes from MAIAK_COOKIE_SECURE (default true); gosec G124 only accepts a literal true.
	c := &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite=Strict are literal, Secure is configurable (default true)
		Name: CookieName, Value: h.cookies.Value(slug, e.PasswordHash, h.clock.Now()), Path: "/status/" + slug,
		MaxAge: int(CookieTTL.Seconds()), HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	}
	http.SetCookie(w, c)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/status/"+slug, http.StatusSeeOther)
}

// sameOrigin refuses a cross-site form post (login CSRF) when the browser tells the origin.
func (h *handler) sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	switch o := r.Header.Get("Origin"); o {
	case "":
		return true
	case "null":
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	default:
		return h.cfg.PublicURL != "" && o == h.cfg.PublicURL
	}
}

func (h *handler) verify(ctx context.Context, password, hash string) bool {
	select {
	case h.argon <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-h.argon }()
	ok, err := crypto.VerifyPassword(password, hash)
	return err == nil && ok
}

// publicOnly serves a JSON, RSS, badge or embed answer only for a page without a password.
func (h *handler) publicOnly(w http.ResponseWriter, r *http.Request) (string, Entry, bool) {
	slug, e, ok := h.lookup(r)
	if !ok || e.PasswordHash != "" {
		h.notFound(w, r)
		return "", Entry{}, false
	}
	return slug, e, true
}

// cached sets the shared-cache headers and answers 304 to a matching If-None-Match.
func cached(w http.ResponseWriter, r *http.Request, etag string) bool {
	tag := `"` + etag + `"`
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "public, max-age=30")
	for _, v := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimSpace(v) == tag || strings.TrimSpace(v) == "W/"+tag {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

func (h *handler) serveJSON(w http.ResponseWriter, r *http.Request) {
	_, e, ok := h.publicOnly(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if cached(w, r, e.ETag) {
		return
	}
	_, _ = w.Write(e.JSON)
}

func (h *handler) serveEmbed(w http.ResponseWriter, r *http.Request) {
	slug, e, ok := h.publicOnly(w, r)
	if !ok {
		return
	}
	src, _ := json.Marshal(h.cfg.PublicURL + "/status/" + slug + "/badge")
	title, _ := json.Marshal("Status of " + e.Snapshot.Page.Title)
	js := "(function(){var s=document.currentScript;if(!s||!s.parentNode)return;" +
		"var f=document.createElement(\"iframe\");f.src=" + string(src) + ";f.title=" + string(title) + ";" +
		"f.width=\"260\";f.height=\"40\";f.loading=\"lazy\";f.referrerPolicy=\"no-referrer\";" +
		"f.setAttribute(\"scrolling\",\"no\");f.style.border=\"0\";f.style.colorScheme=\"normal\";" +
		"s.parentNode.insertBefore(f,s.nextSibling);})();\n"
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	if cached(w, r, e.ETag) {
		return
	}
	_, _ = w.Write([]byte(js))
}

// badgeCSS is the whole style of the badge, sent in its nonce'd element.
const badgeCSS = `:root{color-scheme:light dark}html,body{margin:0;background:transparent}` +
	`a{display:inline-flex;align-items:center;gap:8px;height:30px;padding:0 12px;border:1px solid light-dark(oklch(0.82 0.005 250),oklch(0.32 0.006 250));` +
	`background:light-dark(oklch(0.985 0.002 250),oklch(0.165 0.005 250));color:light-dark(oklch(0.21 0.008 250),oklch(0.93 0.004 250));` +
	`font:500 13px/1 system-ui,-apple-system,"Segoe UI",sans-serif;text-decoration:none;white-space:nowrap}` +
	`a:hover{text-decoration:underline}.shape{display:inline-block;width:9px;height:9px}` +
	`.s-operational{border-radius:50%;background:light-dark(oklch(0.62 0.13 155),oklch(0.7 0.13 155))}` +
	`.s-degraded{background:light-dark(oklch(0.75 0.14 75),oklch(0.8 0.13 75));transform:rotate(45deg) scale(.85)}` +
	`.s-partial_outage,.s-major_outage{background:light-dark(oklch(0.53 0.2 27),oklch(0.72 0.16 27));clip-path:polygon(50% 0,100% 100%,0 100%)}` +
	`.s-no_data{border-radius:50%;border:1.5px solid light-dark(oklch(0.5 0.008 250),oklch(0.66 0.006 250));box-sizing:border-box}`

func (h *handler) serveBadge(w http.ResponseWriter, r *http.Request) {
	slug, e, ok := h.publicOnly(w, r)
	if !ok {
		return
	}
	nonce := newNonce()
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "badge", map[string]any{
		"Nonce": nonce, "BadgeCSS": template.CSS(badgeCSS), "Status": e.Snapshot.Status, "StatusText": overallText(e.Snapshot.Status),
		"PageURL": h.cfg.PublicURL + "/status/" + slug,
	}); err != nil {
		h.notFound(w, r)
		return
	}
	hd := w.Header()
	// The badge is made to be framed by any site; it still runs no script.
	hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+nonce+"'; frame-ancestors *; base-uri 'none'; form-action 'none'")
	hd.Del("X-Frame-Options")
	hd.Set("Cross-Origin-Resource-Policy", "cross-origin")
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("Cache-Control", "public, max-age=30")
	hd.Set("X-Robots-Tag", "noindex, nofollow")
	_, _ = w.Write(buf.Bytes())
}

type rss struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	LastBuild   string    `xml:"lastBuildDate"`
	TTL         int       `xml:"ttl"`
	Items       []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	Description string  `xml:"description"`
	PubDate     string  `xml:"pubDate"`
	GUID        rssGUID `xml:"guid"`
}

type rssGUID struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

func (h *handler) serveRSS(w http.ResponseWriter, r *http.Request) {
	slug, e, ok := h.publicOnly(w, r)
	if !ok {
		return
	}
	s := e.Snapshot
	link := h.cfg.PublicURL + "/status/" + slug
	feed := rss{Version: "2.0", Channel: rssChannel{Title: s.Page.Title + " incidents", Link: link,
		Description: "Incidents of " + s.Page.Title + " (opened and resolved).", Language: "en",
		LastBuild: s.GeneratedAt.UTC().Format(time.RFC1123Z), TTL: 1}}
	add := func(inc statuspages.Incident) {
		feed.Channel.Items = append(feed.Channel.Items, rssItem{
			Title: inc.Title, Link: link + "#incident-" + inc.ID, PubDate: inc.StartedAt.UTC().Format(time.RFC1123Z),
			Description: "Started " + formatTime(inc.StartedAt) + ".", GUID: rssGUID{Value: inc.ID + "-opened"},
		})
		if inc.ResolvedAt != nil {
			feed.Channel.Items = append(feed.Channel.Items, rssItem{
				Title: "Resolved: " + inc.Title, Link: link + "#incident-" + inc.ID, PubDate: inc.ResolvedAt.UTC().Format(time.RFC1123Z),
				Description: "Resolved " + formatTime(*inc.ResolvedAt) + " after " + formatDuration(inc.DurationSeconds) + ".",
				GUID:        rssGUID{Value: inc.ID + "-resolved"},
			})
		}
	}
	for _, inc := range s.ActiveIncidents {
		add(inc)
	}
	for _, inc := range s.PastIncidents {
		add(inc)
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	if cached(w, r, e.ETag) {
		return
	}
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	_ = enc.Encode(feed)
}

// view types of the HTML templates.
type pageView struct {
	Nonce, Title, Description, LogoURL  string
	AccentCSS                           template.CSS
	CSSURL, RSSURL, JSONURL, UnlockURL  string
	Status                              statuspages.Status
	StatusText, Generated, GeneratedISO string
	Groups                              []groupView
	Active, Past                        []incidentView
	NoIndex                             bool
	Error                               string
	imgSrc                              string
}

type groupView struct {
	Name       string
	Components []componentView
}

type componentView struct {
	Name, StatusText, Uptime, Summary string
	Status                            statuspages.Status
	Bars                              []barView
}

type barView struct {
	Status statuspages.Status
	Title  string
}

type incidentView struct {
	Ref, Title, Start, StartISO, End, EndISO, Duration string
}

func (h *handler) pageView(slug string, s statuspages.Snapshot, nonce string) pageView {
	base := "/status/" + slug
	v := pageView{Nonce: nonce, Title: s.Page.Title, Description: s.Page.Description, CSSURL: cssURL(),
		RSSURL: base + "/feed.rss", JSONURL: base + ".json", UnlockURL: base + "/unlock",
		Status: s.Status, StatusText: overallText(s.Status), Generated: formatTime(s.GeneratedAt), GeneratedISO: s.GeneratedAt.UTC().Format(time.RFC3339)}
	if v.Title == "" {
		v.Title = "Status"
	}
	// The accent goes through a strict pattern again before it reaches the style element.
	if s.Page.AccentColor != nil && statuspages.ValidAccent(*s.Page.AccentColor) {
		v.AccentCSS = template.CSS(":root{--accent:" + *s.Page.AccentColor + "}") //nolint:gosec // G203: #rrggbb checked by ValidAccent
	}
	if s.Page.LogoURL != nil {
		if origin, ok := statuspages.LogoOrigin(*s.Page.LogoURL); ok {
			v.LogoURL, v.imgSrc = *s.Page.LogoURL, origin
		}
	}
	var cur *groupView
	for _, c := range s.Components {
		name := ""
		if c.Group != nil {
			name = *c.Group
		}
		if cur == nil || cur.Name != name {
			v.Groups = append(v.Groups, groupView{Name: name})
			cur = &v.Groups[len(v.Groups)-1]
		}
		cv := componentView{Name: c.Name, Status: c.Status, StatusText: componentText(c.Status), Uptime: "No data"}
		if c.Uptime != nil {
			cv.Uptime = formatPercent(*c.Uptime) + " uptime"
		}
		cv.Summary = "Last 90 days: " + cv.Uptime + "."
		for _, d := range c.Days {
			title := formatDate(d.Date) + ": no data"
			if d.Uptime != nil {
				title = formatDate(d.Date) + ": " + formatPercent(*d.Uptime) + " uptime"
			}
			cv.Bars = append(cv.Bars, barView{Status: d.Status, Title: title})
		}
		cur.Components = append(cur.Components, cv)
	}
	for _, inc := range s.ActiveIncidents {
		v.Active = append(v.Active, incidentView{Ref: inc.ID, Title: inc.Title, Start: formatTime(inc.StartedAt),
			StartISO: inc.StartedAt.UTC().Format(time.RFC3339), Duration: formatDuration(inc.DurationSeconds)})
	}
	for _, inc := range s.PastIncidents {
		iv := incidentView{Ref: inc.ID, Title: inc.Title, Start: formatTime(inc.StartedAt),
			StartISO: inc.StartedAt.UTC().Format(time.RFC3339), Duration: formatDuration(inc.DurationSeconds)}
		if inc.ResolvedAt != nil {
			iv.End, iv.EndISO = formatTime(*inc.ResolvedAt), inc.ResolvedAt.UTC().Format(time.RFC3339)
		}
		v.Past = append(v.Past, iv)
	}
	return v
}

func overallText(s statuspages.Status) string {
	switch s {
	case statuspages.Operational:
		return "All systems operational"
	case statuspages.Degraded:
		return "Degraded performance"
	case statuspages.PartialOutage:
		return "Partial outage"
	case statuspages.MajorOutage:
		return "Major outage"
	}
	return "No status data yet"
}

func componentText(s statuspages.Status) string {
	switch s {
	case statuspages.Operational:
		return "Operational"
	case statuspages.Degraded:
		return "Degraded"
	case statuspages.Outage:
		return "Outage"
	}
	return "No data"
}

func formatTime(t time.Time) string { return t.UTC().Format("2 Jan 2006, 15:04") + " UTC" }

func formatDate(d string) string {
	t, err := time.Parse(time.DateOnly, d)
	if err != nil {
		return d
	}
	return t.Format("2 Jan 2006")
}

func formatPercent(p float64) string {
	s := strconv.FormatFloat(p, 'f', 2, 64)
	if p >= 99.995 && p < 100 {
		s = "99.99"
	}
	return s + " %"
}

func formatDuration(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min"
	case d < 48*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return strconv.Itoa(h) + " h"
		}
		return strconv.Itoa(h) + " h " + strconv.Itoa(m) + " min"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days"
}
