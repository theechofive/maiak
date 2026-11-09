-- +goose Up
SET LOCAL lock_timeout = '5s';

-- T-02 contract step (ADR-0102, closes L-285; O-03). Compatibility with the previous app version
-- is intentionally dropped: the production database is reinstalled empty before this release, so
-- no api from before T-02 runs against it. A release from before T-02 inserts monitors without a
-- project and now fails with a NOT NULL violation; rolling the image back past T-02 is therefore
-- no longer supported (forward-only, as always).

-- Locked up front in the application's order (monitor before channel links), so the migration
-- never upgrades a lock halfway; with lock_timeout it fails fast instead of queueing.
LOCK TABLE monitors IN ACCESS EXCLUSIVE MODE;
LOCK TABLE monitor_channels IN ROW EXCLUSIVE MODE;

-- Backfill any monitor still without a project into its tenant's Default project, and delete the
-- stray links of a monitor to another project's channel that the previous release could create
-- (L-286 (5)). The tables force RLS even for their owner, so this runs per tenant under
-- app.tenant_id. On an empty database both statements touch nothing.
-- +goose StatementBegin
DO $$
DECLARE
    v_tenant uuid;
BEGIN
    FOR v_tenant IN SELECT id FROM tenants ORDER BY id LOOP
        PERFORM set_config('app.tenant_id', v_tenant::text, true);
        UPDATE monitors AS m
        SET project_id = p.id
        FROM projects AS p
        WHERE m.tenant_id = v_tenant AND m.project_id IS NULL
          AND p.tenant_id = m.tenant_id AND p.is_default;
        DELETE FROM monitor_channels AS mc
        USING monitors AS m, notification_channels AS c
        WHERE mc.tenant_id = v_tenant
          AND m.tenant_id = mc.tenant_id AND m.id = mc.monitor_id
          AND c.tenant_id = mc.tenant_id AND c.id = mc.channel_id
          AND c.project_id IS NOT NULL AND c.project_id <> m.project_id;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END
$$;
-- +goose StatementEnd

-- Fails (and rolls the whole migration back) if a monitor is still without a project, e.g. a
-- tenant without a Default project; nothing is left half-done.
ALTER TABLE monitors ALTER COLUMN project_id SET NOT NULL;

-- Only the expand step needed these: every caller of the current release sets project_id.
-- tenants_create_default_project and projects_uuidv7 stay: every new tenant still gets its
-- Default project from the database.
DROP TRIGGER monitors_fill_project ON monitors;
DROP FUNCTION monitors_fill_project();
