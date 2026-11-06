package incidents_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/incidents/machine"
	"github.com/theechofive/maiak/backend/internal/monitors"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

// TestIntegrationProjectChannelRouting checks the T-02 routing rule (ADR-0102, closes L-261 and
// L-263): without a selection a monitor notifies the active channels of its own project only;
// a selection may name channels of the monitor's project and organization-wide ones; moving a
// monitor drops the selected channels of its old project.
func TestIntegrationProjectChannelRouting(t *testing.T) {
	f := newFixture(t)
	pgtest.MustExec(t, f.admin, `UPDATE tenants SET plan_key = 'internal' WHERE id = '`+tenantA+`'`)
	web := uuid.New()
	pgtest.MustExec(t, f.admin, `INSERT INTO projects (id, tenant_id, name, slug) VALUES ('`+web.String()+`', '`+tenantA+`', 'Web', 'web')`)
	channel := func(name, project string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		pgtest.MustExec(t, f.admin, `INSERT INTO notification_channels (id, tenant_id, type, config_encrypted, name, project_id)
			VALUES ('`+id.String()+`', '`+tenantA+`', 'slack_webhook', '\x01', '`+name+`', `+project+`)`)
		return id
	}
	inDefault := channel("default", `(SELECT id FROM projects WHERE tenant_id = '`+tenantA+`' AND is_default)`)
	inWeb := channel("web", `'`+web.String()+`'`)
	orgWide := channel("org", "NULL")
	names := map[uuid.UUID]string{inDefault: "default", inWeb: "web", orgWide: "org"}
	opened := func(monitor uuid.UUID) string {
		t.Helper()
		f.apply(monitor, failed("timeout"))
		f.apply(monitor, machine.Result{OK: true})
		rows, err := f.admin.Query(f.ctx, `SELECT channel_id FROM notification_deliveries WHERE monitor_id = $1 AND kind = 'opened' ORDER BY created_at, id`, monitor)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var ch uuid.UUID
			if err := rows.Scan(&ch); err != nil {
				t.Fatal(err)
			}
			out = append(out, names[ch])
		}
		pgtest.MustExec(t, f.admin, `DELETE FROM notification_deliveries WHERE monitor_id = '`+monitor.String()+`'`)
		return strings.Join(out, ",")
	}
	create := func(project *uuid.UUID, sel ...uuid.UUID) (monitors.Monitor, error) {
		url := "https://shop.example.com/" + uuid.NewString()
		in := monitors.CreateInput{Name: "m", URL: &url, FailureThreshold: ptr(1), ProjectID: project}
		if sel != nil {
			in.ChannelIDs = &sel
		}
		return f.monitors.Create(f.ctx, f.actor, in)
	}

	// No selection: the own project's channels only, never organization-wide ones.
	defMon, err := create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := opened(defMon.ID); got != "default" {
		t.Fatalf("Default monitor without selection notified %q", got)
	}
	webMon, err := create(&web)
	if err != nil {
		t.Fatal(err)
	}
	if got := opened(webMon.ID); got != "web" {
		t.Fatalf("Web monitor without selection notified %q", got)
	}

	// A selection: own project and organization-wide channels only.
	// The same answer as an unknown id (T-13, L1).
	if _, err := create(&web, inDefault); err == nil || !strings.Contains(err.Error(), "channelIds: contains a channel that does not exist") {
		t.Fatalf("channel of another project selected: %v", err)
	}
	selMon, err := create(&web, orgWide, inWeb)
	if err != nil {
		t.Fatal(err)
	}
	if got := opened(selMon.ID); got != "web,org" && got != "org,web" {
		t.Fatalf("selection notified %q", got)
	}

	// Moving to the Default project drops the Web channel and keeps the organization-wide one.
	def := uuid.Nil
	if err := f.admin.QueryRow(f.ctx, `SELECT id FROM projects WHERE tenant_id = $1 AND is_default`, tenantA).Scan(&def); err != nil {
		t.Fatal(err)
	}
	moved, err := f.monitors.Update(f.ctx, f.actor, selMon.ID, monitors.UpdateInput{ProjectID: &def})
	if err != nil || moved.ProjectID == nil || *moved.ProjectID != def {
		t.Fatalf("move: %+v %v", moved.ProjectID, err)
	}
	if got := opened(selMon.ID); got != "org" {
		t.Fatalf("after the move notified %q", got)
	}
	// A move with a new selection checks it against the new project.
	if _, err := f.monitors.Update(f.ctx, f.actor, selMon.ID, monitors.UpdateInput{ProjectID: &web, ChannelIDs: &[]uuid.UUID{inDefault}}); err == nil {
		t.Fatal("selection of the old project accepted with the move")
	}
	// A stray link to another project's channel (the previous release, L-286) is not a valid
	// selection: the monitor notifies its own project's channels (T-13).
	strayMon, err := create(&web)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.MustExec(t, f.admin, `INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES ('`+tenantA+`', '`+strayMon.ID.String()+`', '`+inDefault.String()+`')`)
	if got := opened(strayMon.ID); got != "web" {
		t.Fatalf("stray link notified %q", got)
	}

	// An unknown project is a field error.
	unknown := uuid.New()
	if _, err := create(&unknown); err == nil || !strings.Contains(err.Error(), "projectId") {
		t.Fatalf("unknown project: %v", err)
	}
}
