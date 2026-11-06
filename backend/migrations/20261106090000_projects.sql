-- +goose Up
SET LOCAL lock_timeout = '5s';

-- T-02 (ADR-0102): projects inside an organization. Expand step only; the contract step
-- (monitors.project_id NOT NULL, dropping monitors_fill_project) waits for the next release
-- (L-285). The previous app version keeps working:
-- - it never reads projects, project_members or the new columns;
-- - tenants it creates through auth_create_tenant (or by a direct INSERT) get their Default
--   project from tenants_create_default_project;
-- - monitors it creates get the tenant's Default project from monitors_fill_project;
-- - channels it creates have project_id NULL, i.e. they are organization-wide (accepted by
--   ADR-0102, recorded as L-286);
-- - its workers and jobs route deliveries with the previous rule (selected channels, or every
--   active channel) until they are replaced; enqueue_incident_reminders follows the new rule at
--   once (same signature, CREATE OR REPLACE).

CREATE TABLE projects (
    id           uuid        PRIMARY KEY,
    tenant_id    uuid        NOT NULL REFERENCES tenants (id),
    name         text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    slug         text        NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    -- The Default project: one per tenant, never deleted; every member of the organization is a
    -- member of a project with all_members without a project_members row.
    is_default   boolean     NOT NULL DEFAULT false,
    all_members  boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz,
    UNIQUE (tenant_id, id),
    CONSTRAINT projects_default_live CHECK (NOT is_default OR deleted_at IS NULL)
);
CREATE UNIQUE INDEX projects_slug_idx ON projects (tenant_id, slug) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX projects_one_default_idx ON projects (tenant_id) WHERE is_default;
REVOKE DELETE ON projects FROM maiak_api;
REVOKE ALL ON projects FROM maiak_worker;
ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON projects
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Explicit project membership. role overrides the organization role inside the project
-- (member or viewer; NULL = the organization role); the effective role is the lower of the two
-- (ADR-0102 implementation notes). Removing the organization membership removes these rows.
CREATE TABLE project_members (
    tenant_id   uuid        NOT NULL,
    project_id  uuid        NOT NULL,
    user_id     uuid        NOT NULL,
    role        text        CHECK (role IN ('member', 'viewer')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, user_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, user_id) REFERENCES memberships (tenant_id, user_id) ON DELETE CASCADE
);
CREATE INDEX project_members_user_idx ON project_members (tenant_id, user_id);
REVOKE ALL ON project_members FROM maiak_worker;
ALTER TABLE project_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON project_members
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- NULL on notification_channels = organization-wide channel (by design). NULL on monitors exists
-- only until the contract step (L-285).
ALTER TABLE monitors ADD COLUMN project_id uuid;
ALTER TABLE notification_channels ADD COLUMN project_id uuid;

-- A UUIDv7 from the clock, as in enqueue_incident_reminders (the application generates ids
-- elsewhere; the database does only for rows it creates itself).
-- +goose StatementBegin
CREATE FUNCTION projects_uuidv7()
RETURNS uuid
LANGUAGE sql
VOLATILE
SET search_path = public, pg_temp
AS $$
    SELECT encode(set_bit(set_bit(overlay(uuid_send(gen_random_uuid())
               PLACING substring(int8send(floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint) FROM 3)
               FROM 1 FOR 6), 52, 1), 53, 1), 'hex')::uuid
$$;
-- +goose StatementEnd
-- The tenant trigger below runs with the rights of whoever inserts the tenant (maiak_api through
-- the previous release's direct INSERT, L-210).
GRANT EXECUTE ON FUNCTION projects_uuidv7() TO maiak_api;

-- Backfill: a Default project per existing tenant (deleted ones too, so every monitor has a
-- project) and every existing monitor and channel in it, so the empty channel selection of an
-- existing monitor keeps reaching the same channels (ADR-0102). The tables force RLS even for
-- their owner, so this runs per tenant under app.tenant_id.
-- +goose StatementBegin
DO $$
DECLARE
    v_tenant uuid;
    v_project uuid;
BEGIN
    FOR v_tenant IN SELECT id FROM tenants ORDER BY id LOOP
        PERFORM set_config('app.tenant_id', v_tenant::text, true);
        v_project := projects_uuidv7();
        INSERT INTO projects (id, tenant_id, name, slug, is_default, all_members)
        VALUES (v_project, v_tenant, 'Default', 'default', true, true);
        UPDATE monitors SET project_id = v_project WHERE tenant_id = v_tenant AND project_id IS NULL;
        UPDATE notification_channels SET project_id = v_project WHERE tenant_id = v_tenant AND project_id IS NULL;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END
$$;
-- +goose StatementEnd

-- Every new tenant gets its Default project, whoever creates it: auth_create_tenant (registration,
-- BootstrapAdmin), a direct INSERT of the previous release (L-210) or a test fixture. Invoker
-- rights: the insert runs under the tenant isolation policy with app.tenant_id set to the new
-- tenant for the duration of the insert, then restored.
-- +goose StatementBegin
CREATE FUNCTION tenants_create_default_project()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    v_prev text := current_setting('app.tenant_id', true);
BEGIN
    PERFORM set_config('app.tenant_id', NEW.id::text, true);
    INSERT INTO projects (id, tenant_id, name, slug, is_default, all_members)
    VALUES (projects_uuidv7(), NEW.id, 'Default', 'default', true, true);
    PERFORM set_config('app.tenant_id', coalesce(v_prev, ''), true);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER tenants_create_default_project AFTER INSERT ON tenants
    FOR EACH ROW EXECUTE FUNCTION tenants_create_default_project();

-- Monitors created without a project (the previous api during the rollout) land in the tenant's
-- Default project. Dropped by the contract step (L-285).
-- +goose StatementBegin
CREATE FUNCTION monitors_fill_project()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
BEGIN
    SELECT p.id INTO NEW.project_id
    FROM projects AS p
    WHERE p.tenant_id = NEW.tenant_id AND p.is_default;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER monitors_fill_project BEFORE INSERT ON monitors
    FOR EACH ROW WHEN (NEW.project_id IS NULL) EXECUTE FUNCTION monitors_fill_project();

-- Composite keys: a monitor or channel cannot point at another tenant's project. Validating them
-- reads projects across tenants as the owner, which forced RLS hides; a policy that lives only
-- inside this transaction lets the validation see every project.
CREATE POLICY owner_migration_validate ON projects FOR SELECT TO maiak_owner USING (true);
ALTER TABLE monitors ADD CONSTRAINT monitors_project_fk
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id);
ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_project_fk
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id);
DROP POLICY owner_migration_validate ON projects;
-- Both tables are small (at most a few thousand rows per tenant by plan), so the indexes are
-- built inside this transaction instead of CONCURRENTLY.
CREATE INDEX monitors_project_idx ON monitors (tenant_id, project_id) WHERE deleted_at IS NULL;
CREATE INDEX notification_channels_project_idx ON notification_channels (tenant_id, project_id) WHERE deleted_at IS NULL;

-- Plan key max_projects (ADR-0102: a fixed cap of 50, kept in the plans mechanism so a tenant
-- override is possible). The previous release ignores unknown keys.
UPDATE plans SET limits = limits || '{"max_projects": 50}'::jsonb;

-- Reminders follow the T-02 routing rule: a monitor with a selection reaches its selected
-- channels that are organization-wide or of its project; a monitor without one reaches the active
-- channels of its own project only (closes L-261, L-263). Same signature, owner and grants.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enqueue_incident_reminders(p_limit integer)
RETURNS integer
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT pg_advisory_xact_lock(hashtext('maiak.enqueue_incident_reminders'));

    WITH due AS (
        SELECT i.tenant_id, i.id AS incident_id, i.monitor_id, i.started_at, c.id AS channel_id, m.name AS monitor_name
        FROM incidents AS i
        JOIN tenants AS t ON t.id = i.tenant_id
        JOIN monitors AS m ON m.tenant_id = i.tenant_id AND m.id = i.monitor_id
        JOIN notification_channels AS c ON c.tenant_id = i.tenant_id
        WHERE i.resolved_at IS NULL
          AND t.status = 'active' AND t.deleted_at IS NULL
          AND m.notify_slack AND NOT m.paused AND m.deleted_at IS NULL
          AND c.deleted_at IS NULL AND c.status = 'active' AND c.reminder_minutes > 0
          AND 'down' = ANY (c.events)
          AND (
              (EXISTS (SELECT 1 FROM monitor_channels AS mc
                       WHERE mc.tenant_id = i.tenant_id AND mc.monitor_id = m.id AND mc.channel_id = c.id)
               AND (c.project_id IS NULL OR c.project_id = m.project_id))
              OR (NOT EXISTS (SELECT 1 FROM monitor_channels AS mc
                              WHERE mc.tenant_id = i.tenant_id AND mc.monitor_id = m.id)
                  AND c.project_id = m.project_id)
          )
          AND coalesce(
              (SELECT max(d.created_at) FROM notification_deliveries AS d
               WHERE d.incident_id = i.id AND d.channel_id = c.id AND d.kind IN ('opened', 'reminder')),
              i.started_at
          ) <= now() - make_interval(mins => c.reminder_minutes)
        ORDER BY i.started_at, i.id, c.id
        LIMIT least(greatest(p_limit, 0), 1000)
    ),
    queued AS (
        INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, incident_id, channel_id, payload)
        SELECT encode(set_bit(set_bit(overlay(uuid_send(gen_random_uuid())
                   PLACING substring(int8send(floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint) FROM 3)
                   FROM 1 FOR 6), 52, 1), 53, 1), 'hex')::uuid,
               due.tenant_id, 'reminder', due.monitor_id, due.incident_id, due.channel_id,
               jsonb_build_object(
                   'monitorId', due.monitor_id,
                   'monitorName', due.monitor_name,
                   'incidentId', due.incident_id,
                   'startedAt', due.started_at,
                   'durationSeconds', greatest(floor(extract(epoch FROM now() - due.started_at)), 0)::bigint
               )
        FROM due
        RETURNING 1
    )
    SELECT count(*)::integer FROM queued
$$;
-- +goose StatementEnd
