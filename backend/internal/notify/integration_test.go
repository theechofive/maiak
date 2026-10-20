package notify_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/notify"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const (
	tenantA = "01920000-0000-7000-8000-00000000000a"
	tenantB = "01920000-0000-7000-8000-00000000000b"
	userA   = "01920000-0000-7000-8000-0000000000ea"
	urlA    = "https://hooks.slack.com/services/TAAA/BAAA/secretAAAA"
	urlA2   = "https://hooks.slack.com/services/TAAA/BAAA/secretAAA2"
	urlB    = "https://hooks.slack.com/services/TBBB/BBBB/secretBBBB"
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

func TestIntegrationSlackSettings(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("notify"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`,
		`INSERT INTO users (id, email) VALUES ('`+userA+`', 'a@example.com')`)
	sender := &fakeSender{}
	svc := notify.NewService(env.Pool(t, env.APIURL, 4), keyring(t), sender, "https://maiak.example.com")
	a := notify.Actor{TenantID: uuid.MustParse(tenantA), UserID: uuid.MustParse(userA)}
	b := notify.Actor{TenantID: uuid.MustParse(tenantB), UserID: uuid.MustParse(userA)}

	if st, err := svc.Get(ctx, a.TenantID); err != nil || st.Configured {
		t.Fatalf("empty: %+v %v", st, err)
	}
	if err := svc.Test(ctx, a); !errors.Is(err, notify.ErrNotConfigured) {
		t.Fatalf("test unconfigured: %v", err)
	}
	var verr *validation.Error
	if _, err := svc.Put(ctx, a, "https://evil.example/services/x"); !errors.As(err, &verr) || verr.Fields[0].Field != "webhookUrl" {
		t.Fatalf("bad url: %v", err)
	}

	if st, err := svc.Put(ctx, a, urlA); err != nil || !st.Configured || st.Status != notify.StatusActive {
		t.Fatalf("put: %+v %v", st, err)
	}
	if _, err := svc.Put(ctx, b, urlB); err != nil {
		t.Fatal(err)
	}

	// Stored encrypted with the active key: the column is not the URL and names key k2.
	var id uuid.UUID
	var sealed []byte
	if err := admin.QueryRow(ctx, `SELECT id, config_encrypted FROM notification_channels WHERE tenant_id = $1`, tenantA).Scan(&id, &sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hooks.slack.com")) || bytes.Contains(sealed, []byte("secretAAAA")) {
		t.Fatal("config_encrypted contains the URL")
	}
	if kid, err := crypto.KeyID(sealed); err != nil || kid != "k2" {
		t.Fatalf("key id %q %v", kid, err)
	}

	// Replace keeps the id (queued deliveries follow), re-activates a failed channel.
	pgtest.MustExec(t, admin, `UPDATE notification_channels SET status = 'failed' WHERE tenant_id = '`+tenantA+`'`)
	if st, _ := svc.Get(ctx, a.TenantID); st.Status != notify.StatusFailed {
		t.Fatalf("status = %q", st.Status)
	}
	if _, err := svc.Put(ctx, a, urlA2); err != nil {
		t.Fatal(err)
	}
	var n int
	var status string
	if err := admin.QueryRow(ctx, `SELECT count(*), min(status) FROM notification_channels WHERE tenant_id = $1 AND id = $2`, tenantA, id).Scan(&n, &status); err != nil || n != 1 || status != "active" {
		t.Fatalf("replace: n=%d status=%s %v", n, status, err)
	}

	// Test sends to the tenant's own URL only.
	if err := svc.Test(ctx, a); err != nil {
		t.Fatal(err)
	}
	if len(sender.urls) != 1 || sender.urls[0] != urlA2 || !strings.Contains(sender.texts[0], "https://maiak.example.com") {
		t.Fatalf("sent %v %v", sender.urls, sender.texts)
	}
	if err := svc.Test(ctx, b); err != nil || sender.urls[1] != urlB {
		t.Fatalf("tenant b: %v %v", err, sender.urls)
	}

	// A revoked webhook during the test marks the channel failed.
	sender.err = &notify.SendError{Kind: notify.KindNotFound, Status: 404, Permanent: true}
	var se *notify.SendError
	if err := svc.Test(ctx, a); !errors.As(err, &se) || se.Kind != notify.KindNotFound {
		t.Fatalf("test 404: %v", err)
	}
	if st, _ := svc.Get(ctx, a.TenantID); st.Status != notify.StatusFailed {
		t.Fatalf("status after 404 = %q", st.Status)
	}
	if st, _ := svc.Get(ctx, b.TenantID); st.Status != notify.StatusActive {
		t.Fatalf("tenant b affected: %q", st.Status)
	}
	sender.err = &notify.SendError{Kind: notify.KindHTTPStatus, Status: 500}
	if err := svc.Test(ctx, b); err == nil {
		t.Fatal("500 accepted")
	}
	if st, _ := svc.Get(ctx, b.TenantID); st.Status != notify.StatusActive {
		t.Fatalf("500 marked failed: %q", st.Status)
	}

	// Audit rows exist and never contain the URL.
	var audits int
	var meta string
	if err := admin.QueryRow(ctx, `SELECT count(*), string_agg(metadata::text || entity_id, ' ') FROM audit_log WHERE action LIKE 'slack.%'`).Scan(&audits, &meta); err != nil {
		t.Fatal(err)
	}
	if audits != 7 || strings.Contains(meta, "hooks.slack.com") || strings.Contains(meta, "secret") {
		t.Fatalf("audits=%d meta=%s", audits, meta)
	}

	// Tenant isolation: tenant B's transaction cannot resolve tenant A's channel.
	worker := env.Pool(t, env.WorkerURL, 2)
	jobsSvc := notify.NewService(worker, keyring(t), nil, "")
	err := db.WithTenantTx(ctx, worker, b.TenantID, func(tx pgx.Tx) error {
		_, err := jobsSvc.DeliveryTarget(ctx, tx, b.TenantID, id)
		return err
	})
	if !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("cross-tenant lookup: %v", err)
	}
	err = db.WithTenantTx(ctx, worker, a.TenantID, func(tx pgx.Tx) error {
		tg, err := jobsSvc.DeliveryTarget(ctx, tx, a.TenantID, id)
		if err == nil && (tg.WebhookURL != urlA2 || tg.Status != notify.StatusFailed) {
			t.Errorf("target = %+v", tg)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// A ciphertext copied to another row (or tenant) does not decrypt: the AAD binds the row id.
	pgtest.MustExec(t, admin, `UPDATE notification_channels SET config_encrypted = (SELECT config_encrypted FROM notification_channels WHERE tenant_id = '`+tenantA+`') WHERE tenant_id = '`+tenantB+`'`)
	var idB uuid.UUID
	if err := admin.QueryRow(ctx, `SELECT id FROM notification_channels WHERE tenant_id = $1`, tenantB).Scan(&idB); err != nil {
		t.Fatal(err)
	}
	err = db.WithTenantTx(ctx, worker, b.TenantID, func(tx pgx.Tx) error {
		_, err := jobsSvc.DeliveryTarget(ctx, tx, b.TenantID, idB)
		return err
	})
	if !errors.Is(err, notify.ErrUndecryptable) {
		t.Fatalf("copied ciphertext: %v", err)
	}
}
