package migrations_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// rollbackNotificationChannels undoes the two S-01 migrations (ADR-0100), so a test can replay
// an older migration; it is idempotent, for replays that fail before reaching them.
// claim_deliveries is replaced again by the replayed migrations.
var rollbackNotificationChannels = []string{
	`DROP INDEX IF EXISTS notification_deliveries_incident_channel_idx`,
	`DELETE FROM goose_db_version WHERE version_id = 20261027210100`,
	`DROP FUNCTION IF EXISTS enqueue_incident_reminders(integer)`,
	`DROP FUNCTION IF EXISTS fail_deleted_channel_deliveries(uuid)`,
	`DROP POLICY IF EXISTS owner_reminder_read ON incidents`,
	`DROP POLICY IF EXISTS owner_reminder_read ON notification_channels`,
	`DROP POLICY IF EXISTS owner_reminder_read ON monitor_channels`,
	`DROP POLICY IF EXISTS owner_reminder_insert ON notification_deliveries`,
	`DELETE FROM notification_deliveries WHERE kind = 'reminder'`,
	`ALTER TABLE notification_deliveries DROP CONSTRAINT IF EXISTS notification_deliveries_kind_check`,
	`ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_kind_check CHECK (kind IN ('opened', 'resolved', 'tls_expiring'))`,
	`ALTER TABLE notification_channels DROP CONSTRAINT IF EXISTS notification_channels_config_present`,
	`ALTER TABLE notification_channels ALTER COLUMN config_encrypted SET NOT NULL`,
	`ALTER TABLE notification_channels DROP COLUMN IF EXISTS name, DROP COLUMN IF EXISTS events, DROP COLUMN IF EXISTS mention,
		DROP COLUMN IF EXISTS reminder_minutes, DROP COLUMN IF EXISTS deleted_at`,
	`CREATE UNIQUE INDEX IF NOT EXISTS notification_channels_one_slack_idx ON notification_channels (tenant_id) WHERE type = 'slack_webhook'`,
	`DELETE FROM goose_db_version WHERE version_id = 20261027210000`,
}

// TestIntegrationNotificationChannels checks the S-01 migrations (ADR-0100): existing channels
// keep working with defaults, a tenant may have several channels, the column checks, the
// fail_deleted_channel_deliveries and enqueue_incident_reminders functions and the per-channel
// claim order.
func TestIntegrationNotificationChannels(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("notifychannels"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	api := env.Conn(t, env.APIURL)
	worker := env.Conn(t, env.WorkerURL)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)

	// channel inserts a channel of tenant with extra column assignments (SQL) and returns its id.
	channel := func(tenant, cols, vals string) string {
		t.Helper()
		id := uuid.NewString()
		if cols != "" {
			cols, vals = ", "+cols, ", "+vals
		}
		pgtest.MustExec(t, admin, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted`+cols+`)
			VALUES ('`+id+`', '`+tenant+`', 'slack_webhook', '\x01'`+vals+`)`)
		// Since T-02 a channel belongs to a project; these tests use the tenant's Default project,
		// where the T-02 backfill puts every existing channel.
		pgtest.MustExec(t, admin, `DO $$ BEGIN IF to_regclass('public.projects') IS NOT NULL THEN
			UPDATE notification_channels SET project_id = (SELECT p.id FROM projects AS p WHERE p.tenant_id = '`+tenant+`' AND p.is_default)
			WHERE id = '`+id+`'; END IF; END $$`)
		return id
	}

	t.Run("replay over an existing channel", func(t *testing.T) {
		pgtest.MustExec(t, admin, rollbackSchemaVersionGuard...)
		pgtest.MustExec(t, admin, rollbackProjectsContract...)
		pgtest.MustExec(t, admin, rollbackOAuthLogin...)
		pgtest.MustExec(t, admin, rollbackStatusPages...)
		pgtest.MustExec(t, admin, rollbackProjects...)
		pgtest.MustExec(t, admin, rollbackInvitationsRoles...)
		pgtest.MustExec(t, admin, rollbackNotificationChannels...)
		old := channel(tenantA, "", "")
		if n, err := env.Migrate(ctx); err != nil || n != 9 {
			t.Fatalf("re-applied %d migrations, err=%v; want 9 (S-01, T-01, T-02, P-05, P-06, O-01, the T-02 contract and O-06)", n, err)
		}
		var got string
		if err := admin.QueryRow(ctx, `SELECT name || ' ' || events::text || ' ' || mention::text || ' ' || reminder_minutes || ' ' || (deleted_at IS NULL)
			FROM notification_channels WHERE id = $1`, old).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := "Slack {down,recovered,tls_expiring} {} 0 true"; got != want {
			t.Fatalf("existing channel = %q, want %q", got, want)
		}
		var validated bool
		if err := admin.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'notification_deliveries_kind_check'`).Scan(&validated); err != nil || validated {
			t.Fatalf("kind check validated=%v (%v); it is added NOT VALID on purpose", validated, err)
		}
		// Several Slack channels per tenant.
		channel(tenantA, "", "")
		pgtest.MustExec(t, admin, `DELETE FROM notification_channels`)
	})

	t.Run("column checks", func(t *testing.T) {
		for _, c := range []struct{ cols, vals string }{
			{"name", "''"},
			{"name", "'" + strings.Repeat("x", 61) + "'"},
			{"events", "'{}'"},
			{"events", "'{down,agent_offline}'"},
			{"mention", "'{@everyone}'"},
			{"mention", "'{<https://evil|x>}'"},
			{"mention", "'{@here,@channel,<@U123>,<@U456>}'"},
			{"reminder_minutes", "4"},
			{"reminder_minutes", "1441"},
		} {
			id := uuid.NewString()
			_, err := admin.Exec(ctx, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted, `+c.cols+`)
				VALUES ('`+id+`', '`+tenantA+`', 'slack_webhook', '\x01', `+c.vals+`)`)
			if err == nil || !strings.Contains(err.Error(), "check constraint") {
				t.Errorf("%s = %s: err = %v, want a check violation", c.cols, c.vals, err)
			}
		}
		channel(tenantA, "mention, reminder_minutes", "'{@here,<!subteam^S0123ABC>,<@U0ABC>}', 5")
		if _, err := admin.Exec(ctx, `INSERT INTO notification_channels (id, tenant_id, type) VALUES (gen_random_uuid(), '`+tenantA+`', 'slack_webhook')`); err == nil {
			t.Fatal("live channel without config accepted")
		}
		pgtest.MustExec(t, admin, `INSERT INTO notification_channels (id, tenant_id, type, deleted_at) VALUES (gen_random_uuid(), '`+tenantA+`', 'slack_webhook', now())`,
			`DELETE FROM notification_channels`)
	})

	monA := seedMonitor(t, admin, tenantA, "now()", false, false)
	delivery := func(tenant, monitor, channel, kind, incident, age, status string) string {
		t.Helper()
		id := uuid.NewString()
		inc, sent := "NULL", "NULL"
		if incident != "" {
			inc = "'" + incident + "'"
		}
		if status == "sent" {
			sent = "now()"
		}
		pgtest.MustExec(t, admin, `INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, incident_id, channel_id, payload, status, created_at, sent_at)
			VALUES ('`+id+`', '`+tenant+`', '`+kind+`', '`+monitor+`', `+inc+`, '`+channel+`', '{}', '`+status+`', now() - interval '`+age+`', `+sent+`)`)
		return id
	}
	status := func(id string) string {
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

	t.Run("fail_deleted_channel_deliveries", func(t *testing.T) {
		ch := channel(tenantA, "", "")
		other := channel(tenantA, "", "")
		pending := delivery(tenantA, monA, ch, "tls_expiring", "", "1 minute", "pending")
		sent := delivery(tenantA, monA, ch, "tls_expiring", "", "2 minutes", "sent")
		otherPending := delivery(tenantA, monA, other, "tls_expiring", "", "1 minute", "pending")
		call := func(tenant, id string) int {
			t.Helper()
			var n int
			err := pgx.BeginFunc(ctx, api, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
					return err
				}
				return tx.QueryRow(ctx, `SELECT fail_deleted_channel_deliveries($1)`, id).Scan(&n)
			})
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		// A live channel's deliveries are untouched.
		if n := call(tenantA, ch); n != 0 || status(pending) != "pending" {
			t.Fatalf("live channel: failed %d, pending = %s", n, status(pending))
		}
		pgtest.MustExec(t, admin, `UPDATE notification_channels SET deleted_at = now(), config_encrypted = NULL WHERE id = '`+ch+`'`)
		// Another tenant's transaction cannot fail them.
		if n := call(tenantB, ch); n != 0 {
			t.Fatalf("other tenant failed %d", n)
		}
		if n := call(tenantA, ch); n != 1 {
			t.Fatalf("failed %d, want 1", n)
		}
		for id, want := range map[string]string{pending: "failed/channel_deleted", sent: "sent", otherPending: "pending"} {
			if got := status(id); got != want {
				t.Errorf("delivery %s = %s, want %s", id, got, want)
			}
		}
		pgtest.MustExec(t, admin, `DELETE FROM notification_deliveries`)
	})

	t.Run("claim orders per channel", func(t *testing.T) {
		chA := channel(tenantA, "", "")
		chB := channel(tenantA, "", "")
		// The oldest delivery on channel A waits for its retry; channel B's is not held back by it.
		waiting := delivery(tenantA, monA, chA, "tls_expiring", "", "3 minutes", "pending")
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET next_attempt_at = now() + interval '1 hour' WHERE id = '`+waiting+`'`)
		blocked := delivery(tenantA, monA, chA, "tls_expiring", "", "2 minutes", "pending")
		free := delivery(tenantA, monA, chB, "tls_expiring", "", "1 minute", "pending")
		rows, err := worker.Query(ctx, `SELECT id::text FROM claim_deliveries(10)`)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) != 1 || claimed[0] != free {
			t.Fatalf("claimed %v, want only %s (not %s)", claimed, free, blocked)
		}
		pgtest.MustExec(t, admin, `DELETE FROM notification_deliveries`, `DELETE FROM notification_channels`)
	})

	t.Run("enqueue_incident_reminders", func(t *testing.T) {
		incident := func(monitor, tenant, age string) string {
			t.Helper()
			id := uuid.NewString()
			pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO incidents (id, tenant_id, monitor_id, started_at, reason) VALUES ('%s', '%s', '%s', now() - interval '%s', 'timeout')`,
				id, tenant, monitor, age))
			return id
		}
		reminders := func() map[string]int {
			t.Helper()
			if _, err := worker.Exec(ctx, `SELECT enqueue_incident_reminders(100)`); err != nil {
				t.Fatal(err)
			}
			rows, err := admin.Query(ctx, `SELECT channel_id::text || '/' || incident_id::text FROM notification_deliveries WHERE kind = 'reminder' ORDER BY created_at`)
			if err != nil {
				t.Fatal(err)
			}
			keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]int{}
			for _, k := range keys {
				out[k]++
			}
			return out
		}

		every10 := channel(tenantA, "name, reminder_minutes", "'every 10', 10")
		off := channel(tenantA, "name", "'no reminders'")
		// Channels that never get a reminder: no 'down' event, failed, deleted.
		channel(tenantA, "name, events, reminder_minutes", "'recovered only', '{recovered}', 5")
		channel(tenantA, "name, status, reminder_minutes", "'failed', 'failed', 5")
		deleted := channel(tenantA, "name, reminder_minutes", "'deleted', 5")
		pgtest.MustExec(t, admin, `UPDATE notification_channels SET deleted_at = now(), config_encrypted = NULL WHERE id = '`+deleted+`'`)
		otherTenant := channel(tenantB, "name, reminder_minutes", "'tenant b', 5")

		// Down for 25 minutes; opened was sent 25 minutes ago to every10.
		inc := incident(monA, tenantA, "25 minutes")
		delivery(tenantA, monA, every10, "opened", inc, "25 minutes", "sent")
		// Down for 3 minutes on another monitor: not due anywhere yet.
		monFresh := seedMonitor(t, admin, tenantA, "now()", false, false)
		incident(monFresh, tenantA, "3 minutes")
		// Paused monitor and monitor with notifications off: never reminded.
		monPaused := seedMonitor(t, admin, tenantA, "now()", true, false)
		incident(monPaused, tenantA, "1 hour")
		monQuiet := seedMonitor(t, admin, tenantA, "now()", false, false)
		pgtest.MustExec(t, admin, `UPDATE monitors SET notify_slack = false WHERE id = '`+monQuiet+`'`)
		incident(monQuiet, tenantA, "1 hour")
		// A resolved incident.
		resolved := incident(seedMonitor(t, admin, tenantA, "now()", false, false), tenantA, "1 hour")
		pgtest.MustExec(t, admin, `UPDATE incidents SET resolved_at = now(), resolve_reason = 'recovered' WHERE id = '`+resolved+`'`)
		// Tenant B's monitor selects nothing, its tenant is suspended.
		monB := seedMonitor(t, admin, tenantB, "now()", false, false)
		incB := incident(monB, tenantB, "1 hour")
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'suspended' WHERE id = '`+tenantB+`'`)

		got := reminders()
		if len(got) != 1 || got[every10+"/"+inc] != 1 {
			t.Fatalf("reminders = %v, want one for %s/%s", got, every10, inc)
		}
		// The payload carries what the message needs.
		var name string
		var secs int64
		if err := admin.QueryRow(ctx, `SELECT payload->>'monitorName', (payload->>'durationSeconds')::bigint FROM notification_deliveries WHERE kind = 'reminder'`).Scan(&name, &secs); err != nil {
			t.Fatal(err)
		}
		if name != "m" || secs < 25*60 || secs > 26*60 {
			t.Fatalf("payload name=%q duration=%d", name, secs)
		}
		var version int
		if err := admin.QueryRow(ctx, `SELECT get_byte(uuid_send(id), 6) >> 4 FROM notification_deliveries WHERE kind = 'reminder'`).Scan(&version); err != nil || version != 7 {
			t.Fatalf("reminder id version = %d (%v), want 7", version, err)
		}

		// Nothing more until the interval has passed since the last reminder.
		if got := reminders(); got[every10+"/"+inc] != 1 {
			t.Fatalf("second run: %v", got)
		}
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET created_at = now() - interval '11 minutes' WHERE kind = 'reminder'`)
		if got := reminders(); got[every10+"/"+inc] != 2 {
			t.Fatalf("after the interval: %v", got)
		}

		// A selection without the channel excludes it; selecting it includes it again.
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET created_at = now() - interval '11 minutes' WHERE kind = 'reminder'`,
			`INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES ('`+tenantA+`', '`+monA+`', '`+off+`')`)
		if got := reminders(); got[every10+"/"+inc] != 2 {
			t.Fatalf("channel not selected: %v", got)
		}
		pgtest.MustExec(t, admin, `INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES ('`+tenantA+`', '`+monA+`', '`+every10+`')`)
		if got := reminders(); got[every10+"/"+inc] != 3 {
			t.Fatalf("channel selected: %v", got)
		}

		// An active tenant B gets its reminder on its own channel, from the incident start.
		pgtest.MustExec(t, admin, `UPDATE tenants SET status = 'active' WHERE id = '`+tenantB+`'`)
		if got := reminders(); got[otherTenant+"/"+incB] != 1 {
			t.Fatalf("tenant b: %v", got)
		}

		// The function serialises itself: a call blocked by another transaction's lock waits and
		// then queues nothing that the first call queued.
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET created_at = now() - interval '11 minutes' WHERE kind = 'reminder'`)
		holder, err := worker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var first int
		if err := holder.QueryRow(ctx, `SELECT enqueue_incident_reminders(100)`).Scan(&first); err != nil || first != 2 {
			t.Fatalf("first call queued %d (%v), want 2", first, err)
		}
		other := env.Conn(t, env.WorkerURL)
		done := make(chan int, 1)
		go func() {
			var n int
			if err := other.QueryRow(ctx, `SELECT enqueue_incident_reminders(100)`).Scan(&n); err != nil {
				n = -1
			}
			done <- n
		}()
		select {
		case n := <-done:
			t.Fatalf("second call did not wait for the lock: %d", n)
		case <-time.After(200 * time.Millisecond):
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if n := <-done; n != 0 {
			t.Fatalf("second call queued %d after the first committed, want 0", n)
		}

		// The api role cannot queue reminders.
		if _, err := api.Exec(ctx, `SELECT enqueue_incident_reminders(1)`); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("api: %v", err)
		}
	})
}
