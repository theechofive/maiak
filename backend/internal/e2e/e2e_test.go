//go:build maiaktest

package e2e_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/checks"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/jobs"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/safehttp"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
	"github.com/theechofive/maiak/backend/internal/worker"
)

const (
	adminLogin    = "admin"
	adminPassword = "e2e correct horse battery staple"
	publicURL     = "https://maiak.example.com"
	targetHost    = "status.e2e-target.test"
	monitorURL    = "https://" + targetHost + "/health?token=e2e-monitor-secret"
	webhookPath   = "/services/T0E2E/B0E2E/e2eWebhookSecret"
	webhookURL    = "https://" + notify.SlackHost + webhookPath
	monitorName   = "E2E Site"
)

// hostsResolver maps the test hostnames to loopback, standing in for DNS.
type hostsResolver map[string]netip.Addr

func (h hostsResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := h[host]; ok {
		return []netip.Addr{a}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// testCert is a self-signed CA certificate valid for the target and for hooks.slack.com. It is
// valid for a year so no TLS expiry warning is emitted.
type testCert struct {
	cert  tls.Certificate
	roots *x509.CertPool
}

func newTestCert(t *testing.T) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "maiak e2e"},
		DNSNames:              []string{targetHost, notify.SlackHost},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return testCert{cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots: roots}
}

func startTLS(t *testing.T, c testCert, h http.Handler) (*httptest.Server, uint16) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{c.cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, uint16(srv.Listener.Addr().(*net.TCPAddr).Port)
}

// target is the monitored site: it answers with the current status and counts requests.
type target struct {
	status atomic.Int32
	hits   atomic.Int32
}

func (tg *target) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	tg.hits.Add(1)
	w.WriteHeader(int(tg.status.Load()))
}

// fakeSlack records every message with the webhook path it was posted to.
type fakeSlack struct {
	mu    sync.Mutex
	posts []post
}

type post struct{ path, text string }

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct{ Text string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.posts = append(f.posts, post{r.URL.Path, body.Text})
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *fakeSlack) sent() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]post(nil), f.posts...)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type client struct {
	t   *testing.T
	srv *httptest.Server
}

type response struct {
	code   int
	header http.Header
	body   map[string]any
	raw    string
}

func (c *client) do(method, path, body string, hdr map[string]string) response {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := response{code: resp.StatusCode, header: resp.Header, raw: string(b)}
	_ = json.Unmarshal(b, &r.body)
	return r
}

func (c *client) mustDo(want int, method, path, body string, hdr map[string]string) map[string]any {
	c.t.Helper()
	r := c.do(method, path, body, hdr)
	if r.code != want {
		c.t.Fatalf("%s %s: %d %s (want %d)", method, path, r.code, r.raw, want)
	}
	return r.body
}

func TestIntegrationEndToEnd(t *testing.T) {
	start := time.Now()
	env := pgtest.New(t, pgtest.DBName("e2e"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	logs := &lockedBuffer{}
	log := logging.New(logs, -4) // debug: every line the processes could write

	cert := newTestCert(t)
	tgt := &target{}
	tgt.status.Store(http.StatusServiceUnavailable)
	_, targetPort := startTLS(t, cert, tgt)
	slack := &fakeSlack{}
	_, slackPort := startTLS(t, cert, slack)
	loopback := netip.MustParseAddr("127.0.0.1")
	resolver := hostsResolver{targetHost: loopback, notify.SlackHost: loopback}

	// The guarded clients of production with the maiaktest relaxation (loopback and the test
	// port); URL validation, redirects, blocked ranges and the connect-time check are real.
	slackClient := safehttp.NewLoopbackTestClient(safehttp.Options{Timeout: notify.SendTimeout, MaxRedirects: 0, Resolver: resolver}, cert.roots, slackPort)
	probeClient := safehttp.NewLoopbackTestClient(safehttp.Options{Timeout: checks.DefaultTimeout, MaxRedirects: 3, Resolver: resolver}, cert.roots, targetPort)
	notifier := notify.NewSlackNotifier(slackClient)
	keys, err := crypto.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte{9}, 32)}, "k1")
	if err != nil {
		t.Fatal(err)
	}

	// api, as wired in cmd/api.
	apiPool := env.Pool(t, env.APIURL, 4)
	authSvc, err := auth.NewService(apiPool, auth.Options{SessionTTL: time.Hour, Logger: log, Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := crypto.HashPassword(adminPassword, crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	if err := authSvc.BootstrapAdmin(ctx, adminLogin, hash); err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New()
	handler, err := api.NewHandler(api.Config{Version: "e2e", Commit: "e2e", CookieSecure: true}, api.Deps{
		Auth: authSvc, Monitors: monitors.NewService(apiPool, sched, incidents.NewService(sched)),
		Queries: queries.New(apiPool, nil), Slack: notify.NewService(apiPool, keys, notifier, publicURL), Plans: plans.NewService(apiPool),
		Logger: log, Metrics: metrics.NewHTTP(prometheus.NewRegistry()),
	})
	if err != nil {
		t.Fatal(err)
	}
	apiSrv := httptest.NewServer(handler)
	t.Cleanup(apiSrv.Close)
	c := &client{t: t, srv: apiSrv}

	// worker and jobs, as wired in cmd/worker and cmd/jobs (both as maiak_worker).
	workerPool := env.Pool(t, env.WorkerURL, 4)
	newWorker := func(httpClient *http.Client) *worker.Worker {
		return worker.New(worker.Config{ClaimBatch: 10, Concurrency: 4, TenantConcurrency: 2, HostConcurrency: 2}, worker.Deps{
			Pool: workerPool, Scheduler: sched, Incidents: incidents.NewService(sched),
			Prober: checks.NewHTTPSProber(httpClient), Metrics: worker.NewMetrics(prometheus.NewRegistry()), Log: log,
			Jitter: func() time.Duration { return 0 },
		})
	}
	w := newWorker(probeClient)
	if err := w.EnsurePartitions(ctx); err != nil {
		t.Fatal(err)
	}
	jobsReg := prometheus.NewRegistry()
	deliverer := jobs.NewDeliverer(jobs.DeliveryConfig{PublicURL: publicURL, ChannelInterval: time.Millisecond}, jobs.DeliveryDeps{
		Pool: workerPool, Channels: notify.NewService(workerPool, keys, nil, publicURL), Notifier: notifier,
		Metrics: jobs.NewMetrics(jobsReg), Log: log,
	})

	// Monitors are created with next_run_at up to 30 s ahead (jitter); tests make them due now.
	makeDue := func(id string) {
		t.Helper()
		if _, err := admin.Exec(ctx, `UPDATE monitor_location_state SET next_run_at = now() - interval '1 second' WHERE monitor_id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	// Session and CSRF.
	lr := c.do(http.MethodPost, "/api/v1/auth/login", `{"login":"`+adminLogin+`","password":"`+adminPassword+`"}`, nil)
	if lr.code != http.StatusOK {
		t.Fatalf("login: %d %s", lr.code, lr.raw)
	}
	csrf, _ := lr.body["csrf_token"].(string)
	var session string
	for _, ck := range (&http.Response{Header: lr.header}).Cookies() {
		if ck.Name == api.SessionCookie {
			session = ck.Value
		}
	}
	if session == "" || csrf == "" {
		t.Fatalf("login gave no session or CSRF token: %s", lr.raw)
	}
	read := map[string]string{"Cookie": api.SessionCookie + "=" + session}
	unsafe := map[string]string{"Cookie": read["Cookie"], "X-CSRF-Token": csrf}

	t.Run("auth and csrf", func(t *testing.T) {
		if r := c.do(http.MethodGet, "/api/v1/monitors", "", nil); r.code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated list: %d %s", r.code, r.raw)
		}
		if r := c.do(http.MethodPost, "/api/v1/monitors", `{"name":"x","url":"https://example.com"}`, read); r.code != http.StatusForbidden {
			t.Fatalf("create without CSRF header: %d %s", r.code, r.raw)
		}
	})

	// Slack channel through the real URL validation.
	if r := c.do(http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"https://hooks.slack.com.evil.test/services/x"}`, unsafe); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign webhook host: %d %s", r.code, r.raw)
	}
	if s := c.mustDo(http.StatusOK, http.MethodPut, "/api/v1/settings/slack", `{"webhookUrl":"`+webhookURL+`"}`, unsafe); s["configured"] != true || s["status"] != "active" {
		t.Fatalf("slack settings: %v", s)
	}

	// Monitor through the api.
	created := c.mustDo(http.StatusCreated, http.MethodPost, "/api/v1/monitors", `{"name":"`+monitorName+`","url":"`+monitorURL+`"}`, unsafe)
	id, _ := created["id"].(string)
	if created["status"] != "unknown" || created["failureThreshold"] != 2.0 {
		t.Fatalf("created: %v", created)
	}

	checkOnce := func() {
		t.Helper()
		makeDue(id)
		w.RunOnce(ctx)
	}
	checksOf := func(monitor string) []map[string]any {
		t.Helper()
		b := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/monitors/"+monitor+"/checks", "", read)
		var out []map[string]any
		for _, it := range b["items"].([]any) {
			out = append(out, it.(map[string]any))
		}
		return out
	}

	// Two failed checks (threshold 2) open the incident and write the outbox row.
	checkOnce()
	if d := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/monitors/"+id, "", read); d["status"] != "unknown" || d["ongoingIncident"] != nil || d["lastErrorKind"] != "unexpected_status" {
		t.Fatalf("after one failure: %v", d)
	}
	checkOnce()
	if tgt.hits.Load() != 2 {
		t.Fatalf("target hits = %d, want 2", tgt.hits.Load())
	}
	down := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/monitors/"+id, "", read)
	ongoing, _ := down["ongoingIncident"].(map[string]any)
	if down["status"] != "down" || ongoing == nil || ongoing["reason"] != "unexpected_status" {
		t.Fatalf("after two failures: %v", down)
	}
	if len(slack.sent()) != 0 {
		t.Fatal("delivered before the jobs iteration")
	}

	// One jobs iteration delivers the opened message (ADR-0016 format).
	if n := deliverer.RunOnce(ctx); n != 1 {
		t.Fatalf("claimed %d deliveries, want 1", n)
	}
	startedAt := mustTime(t, ongoing["startedAt"])
	wantOpened := ":red_circle: *" + monitorName + "* is down\n" +
		"https://" + targetHost + "/health\n" +
		"Reason: unexpected_status\n" +
		"Since: " + startedAt.UTC().Format("2006-01-02 15:04:05") + " UTC\n" +
		"<" + publicURL + "/monitors/" + id + "|Open in Maiak>"
	if got := slack.sent(); len(got) != 1 || got[0].path != webhookPath || got[0].text != wantOpened {
		t.Fatalf("opened message:\n%+v\nwant %q", got, wantOpened)
	}

	// Recovery resolves the incident; the resolved message follows the opened one.
	tgt.status.Store(http.StatusOK)
	checkOnce()
	if n := deliverer.RunOnce(ctx); n != 1 {
		t.Fatalf("claimed %d deliveries, want 1", n)
	}
	incs := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/monitors/"+id+"/incidents", "", read)["items"].([]any)
	if len(incs) != 1 {
		t.Fatalf("incidents: %v", incs)
	}
	inc := incs[0].(map[string]any)
	resolvedAt := mustTime(t, inc["resolvedAt"])
	duration := resolvedAt.Sub(mustTime(t, inc["startedAt"]))
	if inc["reason"] != "unexpected_status" || !mustTime(t, inc["startedAt"]).Equal(startedAt) || duration <= 0 {
		t.Fatalf("incident: %v", inc)
	}
	wantResolved := fmt.Sprintf(":large_green_circle: *%s* is back up after %ds", monitorName, int64(duration.Seconds()))
	got := slack.sent()
	if len(got) != 2 || got[0].text != wantOpened || got[1].path != webhookPath || got[1].text != wantResolved {
		t.Fatalf("messages:\n%+v\nwant opened then %q", got, wantResolved)
	}
	var sent int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE status = 'sent' AND attempts = 1`).Scan(&sent); err != nil || sent != 2 {
		t.Fatalf("outbox rows sent: %d (%v)", sent, err)
	}

	// Read endpoints agree with what happened.
	cks := checksOf(id)
	if len(cks) != 3 || cks[0]["ok"] != true || cks[0]["httpStatus"] != 200.0 || cks[1]["httpStatus"] != 503.0 ||
		cks[1]["errorKind"] != "unexpected_status" || cks[2]["ok"] != false || cks[0]["location"] != scheduler.HomeLocationCode {
		t.Fatalf("checks: %v", cks)
	}
	if !mustTime(t, cks[0]["checkedAt"]).Equal(resolvedAt) || !mustTime(t, cks[1]["checkedAt"]).Equal(startedAt) {
		t.Fatalf("incident times do not match the checks: %v / %v", inc, cks)
	}
	detail := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/monitors/"+id, "", read)
	uptime, _ := detail["uptime"].(map[string]any)
	if detail["status"] != "up" || detail["ongoingIncident"] != nil || detail["lastErrorKind"] != nil ||
		!mustTime(t, detail["statusSince"]).Equal(resolvedAt) || uptime == nil || !near(uptime["h24"], 100.0/3) {
		t.Fatalf("detail: %v", detail)
	}
	list := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/monitors", "", read)
	items, _ := list["items"].([]any)
	if len(items) != 1 || fmt.Sprint(list["summary"]) != "map[down:0 paused:0 unknown:0 up:1]" {
		t.Fatalf("list: %v", list)
	}
	if it := items[0].(map[string]any); it["status"] != "up" || !near(it["uptime24h"], 100.0/3) || it["lastResponseMs"] == nil ||
		!mustTime(t, it["statusSince"]).Equal(resolvedAt) {
		t.Fatalf("list item: %v", it)
	}
	stats := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/monitors/"+id+"/stats?range=24h", "", read)
	if stats["range"] != "24h" || stats["avgMs"] == nil || stats["p95Ms"] == nil || len(stats["series"].([]any)) == 0 {
		t.Fatalf("stats: %v", stats)
	}

	t.Run("second tenant sees nothing", func(t *testing.T) {
		other := otherTenantSession(t, env)
		if l := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/monitors", "", other); len(l["items"].([]any)) != 0 ||
			fmt.Sprint(l["summary"]) != "map[down:0 paused:0 unknown:0 up:0]" {
			t.Fatalf("other tenant list: %v", l)
		}
		for _, p := range []string{"", "/checks", "/incidents", "/stats"} {
			if r := c.do(http.MethodGet, "/api/v1/monitors/"+id+p, "", other); r.code != http.StatusNotFound {
				t.Errorf("other tenant GET %s: %d %s", p, r.code, r.raw)
			}
		}
		if s := c.mustDo(http.StatusOK, http.MethodGet, "/api/v1/settings/slack", "", other); s["configured"] != false {
			t.Fatalf("other tenant slack settings: %v", s)
		}
	})

	// Paused monitors are not checked.
	if p := c.mustDo(http.StatusOK, http.MethodPost, "/api/v1/monitors/"+id+"/pause", "", unsafe); p["paused"] != true {
		t.Fatalf("pause: %v", p)
	}
	hits := tgt.hits.Load()
	checkOnce()
	if tgt.hits.Load() != hits || len(checksOf(id)) != 3 {
		t.Fatalf("paused monitor was checked: hits %d -> %d", hits, tgt.hits.Load())
	}

	t.Run("normal client refuses the loopback target", func(t *testing.T) {
		b := c.mustDo(http.StatusCreated, http.MethodPost, "/api/v1/monitors", `{"name":"Blocked","url":"https://`+targetHost+`/blocked"}`, unsafe)
		blocked := b["id"].(string)
		makeDue(blocked)
		hits := tgt.hits.Load()
		newWorker(safehttp.NewClient(safehttp.Options{Timeout: checks.DefaultTimeout, MaxRedirects: 3, Resolver: resolver})).RunOnce(ctx)
		cks := checksOf(blocked)
		if len(cks) != 1 || cks[0]["ok"] != false || cks[0]["errorKind"] != "blocked_address" || cks[0]["httpStatus"] != nil {
			t.Fatalf("checks: %v", cks)
		}
		if tgt.hits.Load() != hits {
			t.Fatal("the normal client connected to the loopback target")
		}
	})

	// No secret reached the logs of any process.
	out := logs.String()
	for _, secret := range []string{adminPassword, session, csrf, webhookPath, "e2eWebhookSecret", "e2e-monitor-secret", targetHost} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
	if el := time.Since(start); el > 30*time.Second {
		t.Errorf("end-to-end test took %v, budget 30 s", el)
	}
}

func mustTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, _ := v.(string)
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("not a timestamp: %v", v)
	}
	return ts
}

func near(v any, want float64) bool {
	f, ok := v.(float64)
	return ok && math.Abs(f-want) < 0.01
}

// otherTenantSession creates a second tenant with an owner and a session directly in the
// database (without going through registration) and returns its session headers.
func otherTenantSession(t *testing.T, env *pgtest.Env) map[string]string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	_, err := env.AdminConn(t).Exec(context.Background(), `
		WITH t AS (INSERT INTO tenants (id, slug, name) VALUES ('01920000-0000-7000-8000-0000000000b0', 'other', 'Other') RETURNING id),
		     u AS (INSERT INTO users (id, email) VALUES ('01920000-0000-7000-8000-0000000000b1', 'other@example.com') RETURNING id),
		     m AS (INSERT INTO memberships (tenant_id, user_id, role) SELECT t.id, u.id, 'owner' FROM t, u RETURNING tenant_id, user_id)
		INSERT INTO sessions (id, user_id, tenant_id, token_hash, csrf_token, expires_at)
		SELECT '01920000-0000-7000-8000-0000000000b2', user_id, tenant_id, $1, 'other-csrf', now() + interval '1 hour' FROM m`, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": api.SessionCookie + "=" + token}
}
