package migrations_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackAdminReviewFixes undoes the P-09 migration. It only replaced function bodies, which
// rollbackPlatformAdmin drops or the replayed P-04 migration replaces again.
var rollbackAdminReviewFixes = []string{
	`DELETE FROM goose_db_version WHERE version_id = 20261022090000`,
}

// TestIntegrationAdminReviewFixes checks the P-09 migration (ADR-0098): an unsuspend expires the
// Slack deliveries that waited more than 15 minutes, the claim fails pending deliveries of deleted
// tenants once, and the login tenant prefers an active tenant over a suspended one.
func TestIntegrationAdminReviewFixes(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("adminreviewfixes"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	worker := env.Conn(t, env.WorkerURL)
	seedPlatform(t, admin)

	// delivery inserts a pending delivery of tenant created age ago and returns its id.
	delivery := func(tenant, monitor, channel, age, status string) string {
		t.Helper()
		id := uuid.NewString()
		sent := "NULL"
		if status == "sent" {
			sent = "now()"
		}
		pgtest.MustExec(t, admin, `INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, channel_id, payload, status, created_at, sent_at)
			VALUES ('`+id+`', '`+tenant+`', 'tls_expiring', '`+monitor+`', '`+channel+`', '{}', '`+status+`', now() - interval '`+age+`', `+sent+`)`)
		return id
	}
	state := func(id string) string {
		t.Helper()
		var s string
		var e *string
		if err := admin.QueryRow(ctx, `SELECT status, last_error FROM notification_deliveries WHERE id = $1`, id).Scan(&s, &e); err != nil {
			t.Fatal(err)
		}
		if e != nil {
			s += "/" + *e
		}
		return s
	}
	channels := map[string]string{}
	channelOf := func(tenant string) string {
		t.Helper()
		if id, ok := channels[tenant]; ok {
			return id
		}
		id := uuid.NewString()
		channels[tenant] = id
		pgtest.MustExec(t, admin, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted) VALUES ('`+id+`', '`+tenant+`', 'slack_webhook', '\x01')`)
		return id
	}
	claim := func() int {
		t.Helper()
		var n int
		if err := worker.QueryRow(ctx, `SELECT count(*) FROM claim_deliveries(10)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("an unsuspend expires deliveries older than 15 minutes", func(t *testing.T) {
		monA := seedMonitor(t, admin, tenantA, "now()", false, false)
		monB := seedMonitor(t, admin, tenantB, "now()", false, false)
		chA, chB := channelOf(tenantA), channelOf(tenantB)
		old := delivery(tenantA, monA, chA, "20 minutes", "pending")
		older := delivery(tenantA, monA, chA, "2 hours", "pending")
		sent := delivery(tenantA, monA, chA, "3 hours", "sent")
		fresh := delivery(tenantA, monA, chA, "5 minutes", "pending")
		otherTenant := delivery(tenantB, monB, chB, "1 hour", "pending")
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id IN ('`+tenantA+`', '`+tenantB+`')`)

		var changed bool
		if err := api.QueryRow(ctx, `SELECT admin_set_tenant_status($1, $2, 'active', gen_random_uuid(), NULL)`, userPlatform, tenantA).Scan(&changed); err != nil || !changed {
			t.Fatalf("unsuspend: %v %v", changed, err)
		}
		for id, want := range map[string]string{
			old: "failed/tenant_suspended", older: "failed/tenant_suspended", sent: "sent",
			fresh: "pending", otherTenant: "pending",
		} {
			if got := state(id); got != want {
				t.Errorf("delivery %s = %s, want %s", id, got, want)
			}
		}
		var meta string
		if err := admin.QueryRow(ctx, `SELECT metadata::text FROM audit_log WHERE tenant_id = $1 AND action = 'admin.tenant.unsuspended'`, tenantA).Scan(&meta); err != nil {
			t.Fatal(err)
		}
		if want := `{"to": "active", "from": "suspended", "deliveries_expired": 2}`; meta != want {
			t.Fatalf("audit metadata = %s, want %s", meta, want)
		}
		// The fresh delivery of the unsuspended tenant is claimed; the suspended tenant's waits.
		if n := claim(); n != 1 {
			t.Fatalf("claimed %d, want 1", n)
		}
		if got := state(otherTenant); got != "pending" {
			t.Fatalf("suspended tenant's delivery = %s, want pending", got)
		}
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'active' WHERE id = '`+tenantB+`'`,
			`DELETE FROM notification_deliveries`)
	})

	t.Run("the claim fails pending deliveries of deleted tenants once", func(t *testing.T) {
		monB := seedMonitor(t, admin, tenantB, "now()", false, false)
		chB := channelOf(tenantB)
		pending := delivery(tenantB, monB, chB, "1 minute", "pending")
		sent := delivery(tenantB, monB, chB, "1 hour", "sent")
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended', deleted_at = now() WHERE id = '`+tenantB+`'`)
		if n := claim(); n != 0 {
			t.Fatalf("claimed %d deliveries of a deleted tenant", n)
		}
		if got := state(pending); got != "failed/tenant_deleted" {
			t.Fatalf("pending delivery of a deleted tenant = %s", got)
		}
		if got := state(sent); got != "sent" {
			t.Fatalf("sent delivery = %s", got)
		}
		var left int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE status = 'pending'`).Scan(&left); err != nil || left != 0 {
			t.Fatalf("pending deliveries left: %d (err %v)", left, err)
		}
	})

	t.Run("the login tenant prefers an active tenant over a suspended one", func(t *testing.T) {
		const third = "01920000-0000-7000-8000-0000000000c3"
		pgtest.MustExec(t, admin,
			`INSERT INTO tenants (id, slug, name) VALUES ('`+third+`', 't-cccc', 'Gamma')`,
			`UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantA+`'`,
			// alice is the older member of A (suspended) and a newer member of the active third tenant.
			`INSERT INTO memberships (tenant_id, user_id, role, created_at) VALUES ('`+third+`', '`+userOwnerA+`', 'viewer', now() + interval '1 day')`)
		loginTenant := func() string {
			t.Helper()
			var got *uuid.UUID
			if err := api.QueryRow(ctx, `SELECT auth_login_tenant($1)`, userOwnerA).Scan(&got); err != nil || got == nil {
				t.Fatalf("auth_login_tenant: %v %v", got, err)
			}
			return got.String()
		}
		if got := loginTenant(); got != third {
			t.Fatalf("login tenant = %s, want the active %s", got, third)
		}
		// Back to the oldest membership when both are active; a live suspended one beats a deleted one.
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'active' WHERE id = '`+tenantA+`'`)
		if got := loginTenant(); got != tenantA {
			t.Fatalf("login tenant = %s, want the older %s", got, tenantA)
		}
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended', deleted_at = now() WHERE id = '`+third+`'`,
			`UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantA+`'`)
		if got := loginTenant(); got != tenantA {
			t.Fatalf("login tenant = %s, want the live suspended %s", got, tenantA)
		}
		// The audit row of a password reset goes to the same tenant.
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'active', deleted_at = NULL WHERE id = '`+third+`'`)
		var tenant string
		if err := api.QueryRow(ctx, `SELECT tenant_id::text FROM admin_reset_user_password($1, $2, $3, gen_random_uuid(), NULL)`,
			userPlatform, userOwnerA, fakeHash).Scan(&tenant); err != nil || tenant != third {
			t.Fatalf("reset audit tenant = %s (err %v), want %s", tenant, err, third)
		}
	})

	t.Run("a password reset bumps updated_at", func(t *testing.T) {
		pgtest.MustExec(t, admin, `UPDATE users SET updated_at = now() - interval '1 day' WHERE id = '`+userOwnerA+`'`)
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT * FROM admin_reset_user_password($1, $2, $3, gen_random_uuid(), NULL)`, userPlatform, userOwnerA, fakeHash); err != nil {
			t.Fatal(err)
		}
		var recent bool
		if err := tx.QueryRow(ctx, `SELECT updated_at > now() - interval '1 minute' FROM users WHERE id = $1`, userOwnerA).Scan(&recent); err != nil || !recent {
			t.Fatalf("updated_at bumped = %v (err %v)", recent, err)
		}
	})
}
