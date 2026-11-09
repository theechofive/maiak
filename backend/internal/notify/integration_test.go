package notify_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/notify/notifydb"
	"github.com/theechofive/maiak/backend/internal/plans"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// all is the scope of an owner or admin: every project.
var everyProject = authz.Scope{All: true}

const (
	tenantA  = "01920000-0000-7000-8000-00000000000a"
	tenantB  = "01920000-0000-7000-8000-00000000000b"
	userA    = "01920000-0000-7000-8000-0000000000ea"
	monitorA = "01920000-0000-7000-8000-0000000000a1"
	monitorB = "01920000-0000-7000-8000-0000000000b1"
	urlA     = "https://hooks.slack.com/services/TAAA/BAAA/secretAAAA"
	urlA2    = "https://hooks.slack.com/services/TAAA/BAAA/secretAAA2"
	urlB     = "https://hooks.slack.com/services/TBBB/BBBB/secretBBBB"
)

type fakeSender struct {
	err   error
	urls  []string
	texts []string
}

func (f *fakeSender) Send(_ context.Context, u, text string) error {
	f.urls, f.texts = append(f.urls, u), append(f.texts, text)
	return f.err
}

func keyring(t *testing.T) *crypto.Keyring {
	t.Helper()
	k, err := crypto.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32), "k2": bytes.Repeat([]byte{2}, 32)}, "k2")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func fieldOf(t *testing.T, err error) string {
	t.Helper()
	var verr *validation.Error
	if !errors.As(err, &verr) || len(verr.Fields) == 0 {
		t.Fatalf("err = %v, want a validation error", err)
	}
	return verr.Fields[0].Field
}

func ptr[T any](v T) *T { return &v }

func TestIntegrationChannels(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("notify"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name, plan_key) VALUES ('`+tenantA+`', 'a', 'A', 'internal'), ('`+tenantB+`', 'b', 'B', 'free')`,
		`INSERT INTO users (id, email) VALUES ('`+userA+`', 'a@example.com')`,
		`INSERT INTO monitors (id, tenant_id, project_id, name, config) VALUES
			('`+monitorA+`', '`+tenantA+`', (SELECT id FROM projects WHERE tenant_id = '`+tenantA+`' AND is_default), 'a', '{"url":"https://a.example","expectedStatus":"2xx"}'),
			('`+monitorB+`', '`+tenantB+`', (SELECT id FROM projects WHERE tenant_id = '`+tenantB+`' AND is_default), 'b', '{"url":"https://b.example","expectedStatus":"2xx"}')`)
	sender := &fakeSender{}
	svc := notify.NewService(env.Pool(t, env.APIURL, 4), keyring(t), sender, "https://maiak.example.com")
	a := notify.Actor{TenantID: uuid.MustParse(tenantA), UserID: uuid.MustParse(userA)}
	b := notify.Actor{TenantID: uuid.MustParse(tenantB), UserID: uuid.MustParse(userA)}

	if list, err := svc.List(ctx, a.TenantID, everyProject, nil); err != nil || len(list) != 0 {
		t.Fatalf("empty: %v %v", list, err)
	}
	if err := svc.Test(ctx, a, uuid.New()); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("test unknown: %v", err)
	}

	// Validation: 422 field errors, nothing stored.
	for field, in := range map[string]notify.CreateInput{
		"webhookUrl":      {Name: "x", WebhookURL: "https://evil.example/services/x"},
		"name":            {Name: " ", WebhookURL: urlA},
		"events":          {Name: "x", WebhookURL: urlA, Events: &[]string{}},
		"mention":         {Name: "x", WebhookURL: urlA, Mention: &[]string{"@everyone"}},
		"reminderMinutes": {Name: "x", WebhookURL: urlA, ReminderMinutes: ptr(3)},
	} {
		if _, err := svc.Create(ctx, a, in); fieldOf(t, err) != field {
			t.Errorf("%s: %v", field, err)
		}
	}

	ops, err := svc.Create(ctx, a, notify.CreateInput{Name: " ops ", WebhookURL: urlA})
	if err != nil {
		t.Fatal(err)
	}
	if ops.Name != "ops" || !slices.Equal(ops.Events, notify.AllEvents) || len(ops.Mention) != 0 || ops.ReminderMinutes != 0 ||
		ops.Status != notify.StatusActive || ops.Type != notify.TypeSlack {
		t.Fatalf("defaults: %+v", ops)
	}
	oncall, err := svc.Create(ctx, a, notify.CreateInput{Name: "on-call", WebhookURL: urlA2, Events: &[]string{"down"},
		Mention: &[]string{"@here", "<!subteam^S0ABC>"}, ReminderMinutes: ptr(30)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(oncall.Events, []string{"down"}) || !slices.Equal(oncall.Mention, []string{"@here", "<!subteam^S0ABC>"}) || oncall.ReminderMinutes != 30 {
		t.Fatalf("on-call: %+v", oncall)
	}
	if _, err := svc.Create(ctx, b, notify.CreateInput{Name: "b", WebhookURL: urlB}); err != nil {
		t.Fatal(err)
	}

	// Quota: the free plan allows one channel; a deleted channel frees its slot.
	var lerr *plans.LimitError
	if _, err := svc.Create(ctx, b, notify.CreateInput{Name: "b2", WebhookURL: urlB}); !errors.As(err, &lerr) || lerr.Resource != plans.ResourceChannel || lerr.Max != 1 {
		t.Fatalf("over quota: %v", err)
	}

	// Stored encrypted with the active key, bound to the row id.
	var sealed []byte
	if err := admin.QueryRow(ctx, `SELECT config_encrypted FROM notification_channels WHERE id = $1`, ops.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hooks.slack.com")) || bytes.Contains(sealed, []byte("secretAAAA")) {
		t.Fatal("config_encrypted contains the URL")
	}
	if kid, err := crypto.KeyID(sealed); err != nil || kid != "k2" {
		t.Fatalf("key id %q %v", kid, err)
	}

	// Tenant isolation: tenant B can neither see nor change tenant A's channel.
	if list, _ := svc.List(ctx, b.TenantID, everyProject, nil); len(list) != 1 || list[0].Name != "b" {
		t.Fatalf("tenant b list: %+v", list)
	}
	if _, err := svc.Get(ctx, b.TenantID, everyProject, ops.ID); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("cross-tenant get: %v", err)
	}
	if _, err := svc.Update(ctx, b, ops.ID, notify.UpdateInput{Name: ptr("x")}); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("cross-tenant update: %v", err)
	}
	if err := svc.Delete(ctx, b, ops.ID); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	if err := svc.Test(ctx, b, ops.ID); !errors.Is(err, notify.ErrChannelNotFound) || len(sender.urls) != 0 {
		t.Fatalf("cross-tenant test: %v %v", err, sender.urls)
	}

	// Update: fields change; a new webhook re-activates a failed channel and keeps the id.
	pgtest.MustExec(t, admin, `UPDATE notification_channels SET status = 'failed' WHERE id = '`+ops.ID.String()+`'`)
	upd, err := svc.Update(ctx, a, ops.ID, notify.UpdateInput{Name: ptr("ops-2"), Events: &[]string{"recovered", "down"}, WebhookURL: ptr(urlA2)})
	if err != nil {
		t.Fatal(err)
	}
	if upd.ID != ops.ID || upd.Name != "ops-2" || !slices.Equal(upd.Events, []string{"down", "recovered"}) || upd.Status != notify.StatusActive {
		t.Fatalf("update: %+v", upd)
	}
	if _, err := svc.Update(ctx, a, ops.ID, notify.UpdateInput{Mention: &[]string{"<https://evil|x>"}}); fieldOf(t, err) != "mention" {
		t.Fatalf("bad mention update: %v", err)
	}

	// Test sends to the channel's own URL; a revoked webhook marks it failed.
	if err := svc.Test(ctx, a, oncall.ID); err != nil {
		t.Fatal(err)
	}
	if len(sender.urls) != 1 || sender.urls[0] != urlA2 || !strings.Contains(sender.texts[0], "https://maiak.example.com") {
		t.Fatalf("sent %v %v", sender.urls, sender.texts)
	}
	sender.err = &notify.SendError{Kind: notify.KindNotFound, Status: 404, Permanent: true}
	var se *notify.SendError
	if err := svc.Test(ctx, a, oncall.ID); !errors.As(err, &se) || se.Kind != notify.KindNotFound {
		t.Fatalf("test 404: %v", err)
	}
	if c, _ := svc.Get(ctx, a.TenantID, everyProject, oncall.ID); c.Status != notify.StatusFailed {
		t.Fatalf("status after 404 = %q", c.Status)
	}
	if c, _ := svc.Get(ctx, a.TenantID, everyProject, ops.ID); c.Status != notify.StatusActive {
		t.Fatalf("other channel affected: %q", c.Status)
	}
	sender.err = nil

	// Monitor selection: only live channels of the tenant, at most 10, deduplicated.
	pool := env.Pool(t, env.APIURL, 2)
	selectChannels := func(tenant, monitor string, ids ...uuid.UUID) (bool, error) {
		var changed bool
		err := db.WithTenantTx(ctx, pool, uuid.MustParse(tenant), func(tx pgx.Tx) error {
			var err error
			var project uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT project_id FROM monitors WHERE id = $1`, monitor).Scan(&project); err != nil {
				return err
			}
			changed, err = notify.SetMonitorChannels(ctx, tx, uuid.MustParse(tenant), uuid.MustParse(monitor), project, ids)
			return err
		})
		return changed, err
	}
	channelIDs := func(tenant, monitor string) []uuid.UUID {
		var ids []uuid.UUID
		if err := db.WithTenantTx(ctx, pool, uuid.MustParse(tenant), func(tx pgx.Tx) error {
			var err error
			ids, err = notify.MonitorChannelIDs(ctx, tx, uuid.MustParse(tenant), uuid.MustParse(monitor))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	bList, _ := svc.List(ctx, b.TenantID, everyProject, nil)
	if _, err := selectChannels(tenantA, monitorA, bList[0].ID); fieldOf(t, err) != "channelIds" {
		t.Fatalf("other tenant's channel: %v", err)
	}
	many := make([]uuid.UUID, 11)
	for i := range many {
		many[i] = uuid.New()
	}
	if _, err := selectChannels(tenantA, monitorA, many...); fieldOf(t, err) != "channelIds" {
		t.Fatalf("11 channels: %v", err)
	}
	if changed, err := selectChannels(tenantA, monitorA, ops.ID, oncall.ID, ops.ID); err != nil || !changed {
		t.Fatalf("select: %v %v", changed, err)
	}
	if changed, err := selectChannels(tenantA, monitorA, oncall.ID, ops.ID); err != nil || changed {
		t.Fatalf("same selection: %v %v", changed, err)
	}
	if ids := channelIDs(tenantA, monitorA); len(ids) != 2 {
		t.Fatalf("selection = %v", ids)
	}

	// Delete: monitors detached, pending deliveries failed, webhook wiped, slot freed.
	sent := uuid.New()
	pending := uuid.New()
	pgtest.MustExec(t, admin,
		`INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, channel_id, payload, status, sent_at)
		 VALUES ('`+pending.String()+`', '`+tenantA+`', 'tls_expiring', '`+monitorA+`', '`+ops.ID.String()+`', '{}', 'pending', NULL),
		        ('`+sent.String()+`', '`+tenantA+`', 'tls_expiring', '`+monitorA+`', '`+ops.ID.String()+`', '{}', 'sent', now())`)
	if err := svc.Delete(ctx, a, ops.ID); err != nil {
		t.Fatal(err)
	}
	if ids := channelIDs(tenantA, monitorA); len(ids) != 1 || ids[0] != oncall.ID {
		t.Fatalf("selection after delete = %v", ids)
	}
	var states string
	if err := admin.QueryRow(ctx, `SELECT string_agg(status || '/' || coalesce(last_error, ''), ',' ORDER BY status) FROM notification_deliveries`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != "failed/channel_deleted,sent/" {
		t.Fatalf("deliveries after delete: %s", states)
	}
	var wiped bool
	if err := admin.QueryRow(ctx, `SELECT config_encrypted IS NULL AND deleted_at IS NOT NULL FROM notification_channels WHERE id = $1`, ops.ID).Scan(&wiped); err != nil || !wiped {
		t.Fatalf("deleted channel keeps its webhook: %v %v", wiped, err)
	}
	if _, err := svc.Get(ctx, a.TenantID, everyProject, ops.ID); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
	if err := svc.Delete(ctx, a, ops.ID); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := selectChannels(tenantA, monitorA, ops.ID); fieldOf(t, err) != "channelIds" {
		t.Fatalf("select deleted channel: %v", err)
	}
	// The row writes skip a deleted channel even without the lock (defence in depth: an api pod of
	// the previous release replaces the config by id, L-260).
	var deletedAt time.Time
	if err := admin.QueryRow(ctx, `SELECT deleted_at FROM notification_channels WHERE id = $1`, ops.ID).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.WithTenantTx(ctx, pool, a.TenantID, func(tx pgx.Tx) error {
		q := notifydb.New(tx)
		if err := q.ReplaceChannelConfig(ctx, notifydb.ReplaceChannelConfigParams{TenantID: a.TenantID, ID: ops.ID, ConfigEncrypted: []byte{1}}); err != nil {
			return err
		}
		if err := q.UpdateChannel(ctx, notifydb.UpdateChannelParams{TenantID: a.TenantID, ID: ops.ID, Name: "revived", Events: []string{"down"}, Mention: []string{}}); err != nil {
			return err
		}
		return q.SoftDeleteChannel(ctx, notifydb.SoftDeleteChannelParams{TenantID: a.TenantID, ID: ops.ID})
	}); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err := admin.QueryRow(ctx, `SELECT config_encrypted IS NULL AND status = 'failed' AND name = 'ops-2' AND deleted_at = $2 FROM notification_channels WHERE id = $1`,
		ops.ID, deletedAt).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("deleted channel changed by a direct write: %v %v", untouched, err)
	}
	bOnly := bList[0]
	if err := svc.Delete(ctx, b, bOnly.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, b, notify.CreateInput{Name: "b2", WebhookURL: urlB}); err != nil {
		t.Fatalf("create after delete on free: %v", err)
	}

	// Delivery lookup in jobs: RLS scopes it, a deleted channel is reported without a URL, and a
	// ciphertext copied to another row does not decrypt (the AAD binds the row id).
	worker := env.Pool(t, env.WorkerURL, 2)
	jobsSvc := notify.NewService(worker, keyring(t), nil, "")
	target := func(tenant string, id uuid.UUID) (notify.Target, error) {
		var tg notify.Target
		err := db.WithTenantTx(ctx, worker, uuid.MustParse(tenant), func(tx pgx.Tx) error {
			var err error
			tg, err = jobsSvc.DeliveryTarget(ctx, tx, uuid.MustParse(tenant), id)
			return err
		})
		return tg, err
	}
	if _, err := target(tenantB, oncall.ID); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("cross-tenant lookup: %v", err)
	}
	if tg, err := target(tenantA, oncall.ID); err != nil || tg.WebhookURL != urlA2 || tg.Status != notify.StatusFailed || !slices.Equal(tg.Mention, oncall.Mention) {
		t.Fatalf("target = %+v %v", tg, err)
	}
	if tg, err := target(tenantA, ops.ID); err != nil || !tg.Deleted || tg.WebhookURL != "" {
		t.Fatalf("deleted target = %+v %v", tg, err)
	}
	pgtest.MustExec(t, admin, `UPDATE notification_channels SET config_encrypted = (SELECT config_encrypted FROM notification_channels WHERE id = '`+oncall.ID.String()+`')
		WHERE tenant_id = '`+tenantB+`' AND deleted_at IS NULL`)
	bLive, _ := svc.List(ctx, b.TenantID, everyProject, nil)
	if _, err := target(tenantB, bLive[0].ID); !errors.Is(err, notify.ErrUndecryptable) {
		t.Fatalf("copied ciphertext: %v", err)
	}

	// Audit rows exist and never contain the URL.
	var audits int
	var meta string
	if err := admin.QueryRow(ctx, `SELECT count(*), string_agg(action || metadata::text, ' ') FROM audit_log WHERE action LIKE 'channel.%'`).Scan(&audits, &meta); err != nil {
		t.Fatal(err)
	}
	if audits != 9 || strings.Contains(meta, "hooks.slack.com") || strings.Contains(meta, "secret") ||
		!strings.Contains(meta, `"monitorsDetached": 1`) || !strings.Contains(meta, `"deliveriesFailed": 1`) {
		t.Fatalf("audits=%d meta=%s", audits, meta)
	}
}
