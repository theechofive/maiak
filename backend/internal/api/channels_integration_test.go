package api_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/incidents"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/projects"
	"github.com/theechofive/maiak/backend/internal/queries"
	"github.com/theechofive/maiak/backend/internal/scheduler"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const slackURL = "https://hooks.slack.com/services/T0SECRET/B0SECRET/xoxSECRETTOKEN"

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

type fakeSlack struct {
	mu   sync.Mutex
	err  error
	urls []string
}

func (f *fakeSlack) Send(_ context.Context, u, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, u)
	return f.err
}

// newSlackServer is newServer with the notification channels wired and the log captured.
func newSlackServer(t *testing.T, env *pgtest.Env, sender *fakeSlack, logs *lockedBuffer, clk clock.Clock) *client {
	t.Helper()
	ctx := context.Background()
	pool := env.Pool(t, env.APIURL, 4)
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, err := auth.NewService(pool, auth.Options{SessionTTL: time.Hour, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := crypto.HashPassword(adminPassword, crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BootstrapAdmin(ctx, adminLogin, hash); err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New()
	h, err := api.NewHandler(api.Config{Version: "test", Commit: "test", CookieSecure: true, TrustedProxyHops: 1}, api.Deps{
		Auth: svc, Monitors: monitors.NewService(pool, sched, incidents.NewService(sched)),
		Queries: queries.New(pool, nil), Slack: notify.NewService(pool, keys, sender, "https://maiak.example.com"), Plans: plans.NewService(pool), Projects: projects.NewService(pool),
		Logger: log, Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

// createChannel creates a channel through the API and returns its id.
func createChannel(t *testing.T, c *client, hdr map[string]string, body string) string {
	t.Helper()
	r := c.do(http.MethodPost, "/api/v1/channels", body, hdr)
	if r.code != http.StatusCreated {
		t.Fatalf("create channel: %d %s", r.code, r.raw)
	}
	return r.body["id"].(string)
}

func TestIntegrationChannelsAPI(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apichannels"))
	sender := &fakeSlack{}
	logs := &lockedBuffer{}
	clk := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	c := newSlackServer(t, env, sender, logs, clk)
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	var bodies []string
	// Every call of the test endpoint first waits out the per-tenant limit of one per 5 s.
	record := func(r response) response { bodies = append(bodies, r.raw); clk.Advance(5 * time.Second); return r }

	if r := record(c.do(http.MethodGet, "/api/v1/channels", "", nil)); r.code != http.StatusUnauthorized {
		t.Fatalf("list without session: %d", r.code)
	}
	if r := record(c.do(http.MethodGet, "/api/v1/channels", "", cookie)); r.code != 200 || r.raw != `{"items":[]}`+"\n" {
		t.Fatalf("list empty: %d %q", r.code, r.raw)
	}
	create := `{"name":"ops","webhookUrl":"` + slackURL + `"}`
	if r := record(c.do(http.MethodPost, "/api/v1/channels", create, cookie)); r.code != http.StatusForbidden {
		t.Fatalf("create without CSRF: %d", r.code)
	}

	// Business rules: 422 with the field; the value is never echoed.
	for field, body := range map[string]string{
		"webhookUrl":      `{"name":"x","webhookUrl":"https://evil.example/services/T0SECRET/x"}`,
		"name":            `{"name":"  ","webhookUrl":"` + slackURL + `"}`,
		"reminderMinutes": `{"name":"x","webhookUrl":"` + slackURL + `","reminderMinutes":3}`,
	} {
		r := record(c.do(http.MethodPost, "/api/v1/channels", body, unsafe))
		fe, _ := r.body["fieldErrors"].([]any)
		if r.code != http.StatusUnprocessableEntity || len(fe) != 1 || fe[0].(map[string]any)["field"] != field {
			t.Fatalf("%s: %d %s", field, r.code, r.raw)
		}
	}
	// Contract: unknown fields, unknown events, four mentions, more than a day of reminders,
	// mentions outside the pattern.
	for _, body := range []string{
		`{"name":"x","webhookUrl":"` + slackURL + `","extra":1}`,
		`{"name":"x","webhookUrl":"` + slackURL + `","events":["agent_offline"]}`,
		`{"name":"x","webhookUrl":"` + slackURL + `","events":[]}`,
		`{"name":"x","webhookUrl":"` + slackURL + `","mention":["@here","@channel","<@U1>","<@U2>"]}`,
		`{"name":"x","webhookUrl":"` + slackURL + `","reminderMinutes":1441}`,
		`{"name":"x","webhookUrl":"` + slackURL + `","mention":["<https://evil.example|click>"]}`,
		`{"name":"x","webhookUrl":"` + slackURL + `","mention":["@everyone"]}`,
		`{"name":"x"}`,
	} {
		if r := record(c.do(http.MethodPost, "/api/v1/channels", body, unsafe)); r.code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, r.code, r.raw)
		}
	}

	r := record(c.do(http.MethodPost, "/api/v1/channels", `{"name":"on-call","webhookUrl":"`+slackURL+`","events":["down"],"mention":["@here"],"reminderMinutes":30}`, unsafe))
	if r.code != http.StatusCreated || r.body["name"] != "on-call" || r.body["status"] != "active" || r.body["reminderMinutes"] != 30.0 ||
		fmt.Sprint(r.body["events"]) != "[down]" || fmt.Sprint(r.body["mention"]) != "[@here]" || r.body["type"] != "slack_webhook" {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	id := r.body["id"].(string)
	if _, ok := r.body["webhookUrl"]; ok {
		t.Fatal("webhookUrl returned")
	}
	if g := record(c.do(http.MethodGet, "/api/v1/channels/"+id, "", cookie)); g.code != 200 || g.body["name"] != "on-call" {
		t.Fatalf("get: %d %s", g.code, g.raw)
	}
	if g := record(c.do(http.MethodGet, "/api/v1/channels/"+uuid.NewString(), "", cookie)); g.code != http.StatusNotFound {
		t.Fatalf("get unknown: %d %s", g.code, g.raw)
	}
	if g := record(c.do(http.MethodGet, "/api/v1/channels/nope", "", cookie)); g.code != http.StatusBadRequest {
		t.Fatalf("get malformed id: %d %s", g.code, g.raw)
	}
	p := record(c.do(http.MethodPatch, "/api/v1/channels/"+id, `{"name":"pager","events":["recovered","down"],"mention":[],"reminderMinutes":0}`, unsafe))
	if p.code != 200 || p.body["name"] != "pager" || fmt.Sprint(p.body["events"]) != "[down recovered]" || fmt.Sprint(p.body["mention"]) != "[]" || p.body["reminderMinutes"] != 0.0 {
		t.Fatalf("patch: %d %s", p.code, p.raw)
	}
	second := createChannel(t, c, unsafe, `{"name":"ops","webhookUrl":"https://hooks.slack.com/services/T0SECRET/B0SECRET/xoxSECOND"}`)
	if l := record(c.do(http.MethodGet, "/api/v1/channels", "", cookie)); l.code != 200 || len(l.body["items"].([]any)) != 2 {
		t.Fatalf("list: %d %s", l.code, l.raw)
	}

	// Test sends to the channel's own URL.
	if r := record(c.do(http.MethodPost, "/api/v1/channels/"+id+"/test", "", unsafe)); r.code != http.StatusNoContent {
		t.Fatalf("test: %d %s", r.code, r.raw)
	}
	if len(sender.urls) != 1 || sender.urls[0] != slackURL {
		t.Fatalf("sent to %v", sender.urls)
	}
	if r := record(c.do(http.MethodPost, "/api/v1/channels/"+uuid.NewString()+"/test", "", unsafe)); r.code != http.StatusNotFound {
		t.Fatalf("test unknown: %d %s", r.code, r.raw)
	}
	sender.err = &notify.SendError{Kind: notify.KindHTTPStatus, Status: 500}
	if r := record(c.do(http.MethodPost, "/api/v1/channels/"+id+"/test", "", unsafe)); r.code != http.StatusBadGateway || r.body["type"] != "urn:maiak:problem:slack-delivery-failed" {
		t.Fatalf("test 500: %d %s", r.code, r.raw)
	}
	sender.err = &notify.SendError{Kind: notify.KindRateLimited, Status: 429, RetryAfter: 30 * time.Second}
	if r := record(c.do(http.MethodPost, "/api/v1/channels/"+id+"/test", "", unsafe)); r.code != http.StatusTooManyRequests || r.header.Get("Retry-After") != "30" {
		t.Fatalf("test 429: %d %s", r.code, r.raw)
	}
	sender.err = &notify.SendError{Kind: notify.KindNotFound, Status: 404, Permanent: true}
	if r := record(c.do(http.MethodPost, "/api/v1/channels/"+id+"/test", "", unsafe)); r.code != http.StatusBadGateway {
		t.Fatalf("test 404: %d %s", r.code, r.raw)
	}
	sender.err = nil
	if g := record(c.do(http.MethodGet, "/api/v1/channels/"+id, "", cookie)); g.body["status"] != "failed" {
		t.Fatalf("status after 404: %s", g.raw)
	}
	if p := record(c.do(http.MethodPatch, "/api/v1/channels/"+id, `{"webhookUrl":"`+slackURL+`"}`, unsafe)); p.code != 200 || p.body["status"] != "active" {
		t.Fatalf("new webhook: %d %s", p.code, p.raw)
	}

	// Monitors select channels; only channels of the tenant, at most 10.
	m := record(c.do(http.MethodPost, "/api/v1/monitors", `{"name":"Shop","url":"https://example.com","channelIds":["`+id+`"]}`, unsafe))
	if m.code != http.StatusCreated || fmt.Sprint(m.body["channelIds"]) != "["+id+"]" {
		t.Fatalf("monitor with channel: %d %s", m.code, m.raw)
	}
	monitor := m.body["id"].(string)
	if u := record(c.do(http.MethodPatch, "/api/v1/monitors/"+monitor, `{"channelIds":["`+uuid.NewString()+`"]}`, unsafe)); u.code != http.StatusUnprocessableEntity ||
		!strings.Contains(u.raw, `"field":"channelIds"`) {
		t.Fatalf("unknown channel: %d %s", u.code, u.raw)
	}
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = `"` + uuid.NewString() + `"`
	}
	if u := record(c.do(http.MethodPatch, "/api/v1/monitors/"+monitor, `{"channelIds":[`+strings.Join(eleven, ",")+`]}`, unsafe)); u.code != http.StatusBadRequest {
		t.Fatalf("eleven channels: %d %s", u.code, u.raw)
	}
	if u := record(c.do(http.MethodPatch, "/api/v1/monitors/"+monitor, `{"channelIds":["`+id+`","`+second+`"]}`, unsafe)); u.code != 200 || len(u.body["channelIds"].([]any)) != 2 {
		t.Fatalf("two channels: %d %s", u.code, u.raw)
	}

	// Delete detaches the monitor and makes the channel unknown.
	if d := record(c.do(http.MethodDelete, "/api/v1/channels/"+id, "", cookie)); d.code != http.StatusForbidden {
		t.Fatalf("delete without CSRF: %d", d.code)
	}
	if d := record(c.do(http.MethodDelete, "/api/v1/channels/"+id, "", unsafe)); d.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", d.code, d.raw)
	}
	if g := record(c.do(http.MethodGet, "/api/v1/monitors/"+monitor, "", cookie)); fmt.Sprint(g.body["channelIds"]) != "["+second+"]" {
		t.Fatalf("monitor after delete: %s", g.raw)
	}
	for _, rq := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/channels/" + id}, {http.MethodDelete, "/api/v1/channels/" + id},
		{http.MethodPost, "/api/v1/channels/" + id + "/test"},
	} {
		if r := record(c.do(rq.method, rq.path, "", unsafe)); r.code != http.StatusNotFound {
			t.Fatalf("%s %s after delete: %d %s", rq.method, rq.path, r.code, r.raw)
		}
	}
	if p := record(c.do(http.MethodPatch, "/api/v1/channels/"+id, `{"name":"x"}`, unsafe)); p.code != http.StatusNotFound {
		t.Fatalf("patch after delete: %d %s", p.code, p.raw)
	}

	// The URL never appears in a response or a log line.
	all := strings.Join(bodies, "\n") + logs.String()
	for _, secret := range []string{"T0SECRET", "xoxSECRETTOKEN", "xoxSECOND"} {
		if strings.Contains(all, secret) {
			t.Fatalf("%s leaked in responses or logs:\n%s", secret, all)
		}
	}
	if logs.String() == "" {
		t.Fatal("no log output captured")
	}
}

// POST /channels/{id}/test is limited to one call per 5 s per tenant, whatever the outcome.
func TestIntegrationChannelTestRateLimit(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apichannellimit"))
	sender := &fakeSlack{}
	clk := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	c := newSlackServer(t, env, sender, &lockedBuffer{}, clk)
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	id := createChannel(t, c, unsafe, `{"name":"ops","webhookUrl":"`+slackURL+`"}`)
	other := createChannel(t, c, unsafe, `{"name":"ops 2","webhookUrl":"`+slackURL+`"}`)
	test := func(hdr map[string]string, channel string) response {
		return c.do(http.MethodPost, "/api/v1/channels/"+channel+"/test", "", hdr)
	}
	if r := test(unsafe, id); r.code != http.StatusNoContent {
		t.Fatalf("first test: %d %s", r.code, r.raw)
	}
	clk.Advance(2 * time.Second)
	// The limit is per tenant: another channel of the same tenant waits too.
	r := test(unsafe, other)
	if r.code != http.StatusTooManyRequests || r.body["type"] != "urn:maiak:problem:rate-limited" || r.header.Get("Retry-After") != "3" ||
		!strings.HasPrefix(r.header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("second test within 5 s: %d %v %s", r.code, r.header, r.raw)
	}
	if len(sender.urls) != 1 {
		t.Fatalf("limited call reached Slack: %d sends", len(sender.urls))
	}
	// Another tenant has its own budget (its call reaches the handler: 404, not its channel).
	otherCookie, otherCSRF := otherTenantSession(t, env)
	if r := test(withCSRF(otherCookie, otherCSRF), id); r.code != http.StatusNotFound {
		t.Fatalf("other tenant: %d %s", r.code, r.raw)
	}
	clk.Advance(3 * time.Second)
	if r := test(unsafe, other); r.code != http.StatusNoContent || len(sender.urls) != 2 {
		t.Fatalf("after 5 s: %d %s, %d sends", r.code, r.raw, len(sender.urls))
	}
}

// Creating a channel counts against the channel quota; deleted channels do not count.
func TestIntegrationChannelQuota(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("apichannelquota"))
	c := newSlackServer(t, env, &fakeSlack{}, &lockedBuffer{}, nil)
	cookie, csrf := c.login()
	unsafe := withCSRF(cookie, csrf)
	admin := env.AdminConn(t)
	create := func() response {
		return c.do(http.MethodPost, "/api/v1/channels", `{"name":"ops","webhookUrl":"`+slackURL+`"}`, unsafe)
	}

	pgtest.MustExec(t, admin, `UPDATE tenants SET plan_key = 'free', limits = '{"max_channels": 0}' WHERE slug = 'default'`)
	r := create()
	if r.code != http.StatusConflict || r.body["type"] != "urn:maiak:problem:plan-limit-reached" ||
		r.body["limit"] != "channels" || r.body["max"] != float64(0) || r.body["detail"] != "The Free plan allows at most 0 channels." {
		t.Fatalf("new channel over quota: %d %s", r.code, r.raw)
	}

	// The free plan allows one channel; a deleted one frees its slot.
	pgtest.MustExec(t, admin, `UPDATE tenants SET limits = '{}' WHERE slug = 'default'`)
	first := create()
	if first.code != http.StatusCreated {
		t.Fatalf("first on free: %d %s", first.code, first.raw)
	}
	if r := create(); r.code != http.StatusConflict || r.body["max"] != float64(1) {
		t.Fatalf("second on free: %d %s", r.code, r.raw)
	}
	u := c.do(http.MethodGet, "/api/v1/usage", "", cookie)
	if u.code != http.StatusOK || !strings.Contains(u.raw, `"usage":{"channels":1,"heartbeats":0,"members":1,"monitors":0,"projects":1}`) ||
		!strings.Contains(u.raw, `"limits":{"channels":1,"heartbeats":1,"members":3,"monitors":3,"projects":50}`) {
		t.Fatalf("usage: %d %s", u.code, u.raw)
	}
	if d := c.do(http.MethodDelete, "/api/v1/channels/"+first.body["id"].(string), "", unsafe); d.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", d.code, d.raw)
	}
	if r := create(); r.code != http.StatusCreated {
		t.Fatalf("create after delete: %d %s", r.code, r.raw)
	}
	var n int
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM notification_channels WHERE deleted_at IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("live channels = %d (%v)", n, err)
	}
}
