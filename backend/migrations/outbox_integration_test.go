package migrations_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationOutbox checks claim_deliveries and finish_delivery: per-monitor ordering, at
// most one delivery in flight per monitor, lease expiry, retries, and the grants around them.
func TestIntegrationOutbox(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("outbox"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	worker := env.Conn(t, env.WorkerURL)
	api := env.Conn(t, env.APIURL)
	pgtest.MustExec(t, admin, `INSERT INTO tenants (id, slug, name) VALUES ('`+tenantA+`', 'a', 'A'), ('`+tenantB+`', 'b', 'B')`)
	m1 := seedMonitor(t, admin, tenantA, "now()", false, false)
	m2 := seedMonitor(t, admin, tenantB, "now()", false, false)
	channelA, channelB := uuid.New().String(), uuid.New().String()
	incident := uuid.New().String()
	pgtest.MustExec(t, admin,
		`INSERT INTO notification_channels (id, tenant_id, type, config_encrypted) VALUES
			('`+channelA+`', '`+tenantA+`', 'slack_webhook', '\x01'), ('`+channelB+`', '`+tenantB+`', 'slack_webhook', '\x01')`,
		`INSERT INTO incidents (id, tenant_id, started_at, reason) VALUES ('`+incident+`', '`+tenantA+`', now(), 'timeout')`)

	// Deliveries are created in order: opened then resolved for m1, opened for m2.
	deliver := func(tenant, monitor, channel, kind string, incidentID *string) string {
		id := uuid.Must(uuid.NewV7()).String()
		inc := "NULL"
		if incidentID != nil {
			inc = "'" + *incidentID + "'"
		}
		pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, incident_id, channel_id, payload)
			VALUES ('%s', '%s', '%s', '%s', %s, '%s', '{"monitorName":"m"}')`, id, tenant, kind, monitor, inc, channel))
		return id
	}
	opened1 := deliver(tenantA, m1, channelA, "opened", &incident)
	resolved1 := deliver(tenantA, m1, channelA, "resolved", &incident)
	tls2 := deliver(tenantB, m2, channelB, "tls_expiring", nil)

	type claimed struct{ id, monitor, kind, token string }
	// tokens holds the claim token of the latest claim of each row.
	tokens := map[string]string{}
	claimOn := func(q interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}, n int) []claimed {
		t.Helper()
		rows, _ := q.Query(ctx, `SELECT id::text, monitor_id::text, kind, claim_token::text FROM claim_deliveries($1) ORDER BY created_at, id`, n)
		out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claimed, error) {
			var c claimed
			return c, r.Scan(&c.id, &c.monitor, &c.kind, &c.token)
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		for _, c := range out {
			tokens[c.id] = c.token
		}
		return out
	}
	ids := func(cs []claimed) []string {
		out := []string{}
		for _, c := range cs {
			out = append(out, c.id)
		}
		return out
	}
	finish := func(id, status, errText, retry string) bool {
		t.Helper()
		var ok bool
		if err := worker.QueryRow(ctx, `SELECT finish_delivery($1, $2, $3, $4, `+retry+`)`, id, tokens[id], status, errText).Scan(&ok); err != nil {
			t.Fatalf("finish %s: %v", status, err)
		}
		return ok
	}

	t.Run("oldest per monitor only, one in flight per monitor", func(t *testing.T) {
		if got := ids(claimOn(worker, 10)); !slices.Equal(got, []string{opened1, tls2}) {
			t.Fatalf("claimed %v, want opened of m1 and tls of m2", got)
		}
		if got := claimOn(worker, 10); len(got) != 0 {
			t.Fatalf("claimed %v while the older row of m1 is in flight", got)
		}
	})

	t.Run("a row waiting for its retry blocks newer rows of the monitor", func(t *testing.T) {
		if !finish(opened1, "pending", "slack 500", "now() + interval '1 hour'") {
			t.Fatal("finish pending returned false")
		}
		if got := claimOn(worker, 10); len(got) != 0 {
			t.Fatalf("claimed %v; resolved must wait for opened", got)
		}
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET next_attempt_at = now() WHERE id = '`+opened1+`'`)
		if got := ids(claimOn(worker, 10)); !slices.Equal(got, []string{opened1}) {
			t.Fatalf("claimed %v, want the retried opened row", got)
		}
		if !finish(opened1, "sent", "", "NULL") {
			t.Fatal("finish sent returned false")
		}
		if finish(opened1, "sent", "", "NULL") {
			t.Fatal("finishing a row that is not in progress returned true")
		}
		var attempts int
		var sent, lastErr bool
		if err := admin.QueryRow(ctx, `SELECT attempts, sent_at IS NOT NULL, last_error IS NOT NULL FROM notification_deliveries WHERE id = $1`,
			opened1).Scan(&attempts, &sent, &lastErr); err != nil || attempts != 2 || !sent || lastErr {
			t.Fatalf("after retry and send: attempts=%d sent=%t lastError=%t err=%v", attempts, sent, lastErr, err)
		}
		if got := ids(claimOn(worker, 10)); !slices.Equal(got, []string{resolved1}) {
			t.Fatalf("claimed %v, want resolved after opened was sent", got)
		}
	})

	t.Run("a dead claimer's rows return after the lease", func(t *testing.T) {
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET lease_until = now() - interval '1 second' WHERE id = '`+resolved1+`'`)
		if got := ids(claimOn(worker, 10)); !slices.Equal(got, []string{resolved1}) {
			t.Fatalf("claimed %v, want the expired in-progress row", got)
		}
		if !finish(resolved1, "failed", strings.Repeat("x", 600), "NULL") {
			t.Fatal("finish failed returned false")
		}
		var status string
		var errLen int
		if err := admin.QueryRow(ctx, `SELECT status, length(last_error) FROM notification_deliveries WHERE id = $1`, resolved1).Scan(&status, &errLen); err != nil ||
			status != "failed" || errLen != 500 {
			t.Fatalf("status %s, error length %d (err %v)", status, errLen, err)
		}
	})

	t.Run("concurrent claimers get disjoint rows", func(t *testing.T) {
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET status = 'sent', sent_at = now(), lease_until = NULL, claim_token = NULL WHERE status = 'in_progress'`)
		var want []string
		for range 5 {
			want = append(want, deliver(tenantA, seedMonitor(t, admin, tenantA, "now()", false, false), channelA, "tls_expiring", nil))
		}
		w2 := env.Conn(t, env.WorkerURL)
		tx1, err := worker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		tx2, err := w2.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		a, b := ids(claimOn(tx1, 2)), ids(claimOn(tx2, 10))
		got := append(a, b...)
		slices.Sort(got)
		slices.Sort(want)
		if len(a) != 2 || !slices.Equal(got, want) {
			t.Fatalf("claimed %v and %v, want disjoint sets covering %v", a, b, want)
		}
	})

	// Settle everything so far: the subtests below claim only their own rows.
	settle := func() {
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET status = 'sent', sent_at = now(), lease_until = NULL, claim_token = NULL
			WHERE status IN ('pending', 'in_progress')`)
	}
	row := func(id string) (status string, attempts int, lastErr *string) {
		t.Helper()
		if err := admin.QueryRow(ctx, `SELECT status, attempts, last_error FROM notification_deliveries WHERE id = $1`, id).Scan(&status, &attempts, &lastErr); err != nil {
			t.Fatal(err)
		}
		return status, attempts, lastErr
	}
	expire := func(id string) {
		pgtest.MustExec(t, admin, `UPDATE notification_deliveries SET lease_until = now() - interval '1 second' WHERE id = '`+id+`'`)
	}

	t.Run("finish with the token of an expired claim is rejected", func(t *testing.T) {
		settle()
		id := deliver(tenantA, seedMonitor(t, admin, tenantA, "now()", false, false), channelA, "tls_expiring", nil)
		if got := ids(claimOn(worker, 10)); !slices.Equal(got, []string{id}) {
			t.Fatalf("claimed %v", got)
		}
		first := tokens[id]
		expire(id)
		if got := ids(claimOn(worker, 10)); !slices.Equal(got, []string{id}) || tokens[id] == first {
			t.Fatalf("reclaim %v with token %s (first %s)", got, tokens[id], first)
		}
		var ok bool
		if err := worker.QueryRow(ctx, `SELECT finish_delivery($1, $2, 'sent', NULL, NULL)`, id, first).Scan(&ok); err != nil || ok {
			t.Fatalf("finish with the stale token = %t (err %v), want false", ok, err)
		}
		if status, attempts, lastErr := row(id); status != "in_progress" || attempts != 1 || lastErr == nil || *lastErr != "lease_expired" {
			t.Fatalf("after the stale finish: %s attempts=%d last_error=%v; want in_progress, the requeue counted", status, attempts, lastErr)
		}
		if !finish(id, "sent", "", "NULL") {
			t.Fatal("finish with the current token returned false")
		}
	})

	t.Run("a poison delivery ends failed after the maximum attempts", func(t *testing.T) {
		settle()
		id := deliver(tenantA, seedMonitor(t, admin, tenantA, "now()", false, false), channelA, "tls_expiring", nil)
		const maxAttempts = 5 // jobs.MaxAttempts; the jobs integration test checks the pair
		for i := range maxAttempts {
			got := ids(claimOn(worker, 10))
			if !slices.Equal(got, []string{id}) {
				t.Fatalf("claim %d: %v", i+1, got)
			}
			expire(id) // the jobs process dies with the row in flight
		}
		if got := claimOn(worker, 10); len(got) != 0 {
			t.Fatalf("poison row claimed again: %v", got)
		}
		if status, attempts, lastErr := row(id); status != "failed" || attempts != maxAttempts || lastErr == nil || *lastErr != "lease_expired" {
			t.Fatalf("poison row: %s attempts=%d last_error=%v", status, attempts, lastErr)
		}
	})

	t.Run("grants and arguments", func(t *testing.T) {
		for _, stmt := range []string{`SELECT * FROM claim_deliveries(1)`, `SELECT finish_delivery(gen_random_uuid(), gen_random_uuid(), 'sent', '', NULL)`} {
			if _, err := api.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("api %s: err = %v, want permission denied", stmt, err)
			}
		}
		for _, stmt := range []string{`SELECT finish_delivery(gen_random_uuid(), gen_random_uuid(), 'bogus', '', NULL)`, `SELECT finish_delivery(gen_random_uuid(), gen_random_uuid(), 'pending', '', NULL)`} {
			if _, err := worker.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "finish_delivery") {
				t.Errorf("worker %s: err = %v, want an argument error", stmt, err)
			}
		}
		if _, err := execInTenant(t, worker, tenantA, `UPDATE notification_deliveries SET status = 'sent'`); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("worker updates deliveries directly: err = %v", err)
		}
		if _, err := execInTenant(t, api, tenantA, `INSERT INTO incidents (id, tenant_id, started_at) VALUES (gen_random_uuid(), '`+tenantA+`', now())`); err == nil ||
			!strings.Contains(err.Error(), "permission denied") {
			t.Errorf("api opens an incident: err = %v", err)
		}
		if _, err := execInTenant(t, worker, tenantA, `UPDATE notification_channels SET config_encrypted = '\x02'`); err == nil ||
			!strings.Contains(err.Error(), "permission denied") {
			t.Errorf("worker rewrites a channel: err = %v", err)
		}
		if n := countInTenant(t, worker, tenantB, `SELECT count(*) FROM notification_deliveries WHERE tenant_id = '`+tenantA+`'`); n != 0 {
			t.Errorf("tenant B sees %d deliveries of tenant A", n)
		}
		if _, err := execInTenant(t, api, tenantA, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted)
			VALUES (gen_random_uuid(), '`+tenantA+`', 'slack_webhook', '\x01')`); err == nil || !strings.Contains(err.Error(), "notification_channels_one_slack_idx") {
			t.Errorf("second Slack channel: err = %v, want the unique index", err)
		}
	})
}
