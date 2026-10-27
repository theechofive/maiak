-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Notification channels (S-01, ADR-0100, supersedes ADR-0019): several Slack webhook channels per
-- tenant, each with a name, the events it receives, an optional mention and an optional reminder
-- interval; monitors choose their channels through monitor_channels; deleted channels are kept
-- (soft delete) because notification_deliveries references them, but lose their webhook.
--
-- Expand/contract caveats with the previous release during the rollout (ADR-0100):
-- - the previous api creates a first Slack channel with ON CONFLICT on the dropped
--   one-channel index, which now fails (500); replacing an existing webhook still works;
-- - a deleted channel is also marked 'failed', so the previous worker (which picks the tenant's
--   first active Slack channel) and jobs (which skip failed channels) never use it;
-- - the previous jobs cannot format a 'reminder' delivery and marks it failed (bad_payload).

-- New columns have constant defaults (no table rewrite); existing channels get the name "Slack",
-- every event and no mention or reminder, which is what they did before.
ALTER TABLE notification_channels
    ADD COLUMN name             text        NOT NULL DEFAULT 'Slack' CHECK (length(name) BETWEEN 1 AND 60),
    ADD COLUMN events           text[]      NOT NULL DEFAULT ARRAY['down', 'recovered', 'tls_expiring']::text[]
        CHECK (cardinality(events) BETWEEN 1 AND 3 AND events <@ ARRAY['down', 'recovered', 'tls_expiring']::text[]),
    -- Slack mention tokens as typed by the user (@here, @channel, <!subteam^ID>, <@UID>), at most
    -- three; the application validates them strictly, the check keeps anything else out.
    ADD COLUMN mention          text[]      NOT NULL DEFAULT '{}'::text[]
        CHECK (cardinality(mention) <= 3
               AND array_to_string(mention, ' ') ~ '^((@here|@channel|<!subteam\^[A-Z0-9]{2,20}>|<@[UW][A-Z0-9]{2,20}>)( |$))*$'),
    ADD COLUMN reminder_minutes integer     NOT NULL DEFAULT 0
        CHECK (reminder_minutes = 0 OR reminder_minutes BETWEEN 5 AND 1440),
    ADD COLUMN deleted_at       timestamptz;

-- Deleting a channel wipes its encrypted webhook URL.
ALTER TABLE notification_channels ALTER COLUMN config_encrypted DROP NOT NULL;
ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_config_present
    CHECK (deleted_at IS NOT NULL OR config_encrypted IS NOT NULL) NOT VALID;
ALTER TABLE notification_channels VALIDATE CONSTRAINT notification_channels_config_present;

DROP INDEX notification_channels_one_slack_idx;

-- Reminders of open incidents.
ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_kind_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_kind_check
    CHECK (kind IN ('opened', 'resolved', 'tls_expiring', 'reminder')) NOT VALID;
ALTER TABLE notification_deliveries VALIDATE CONSTRAINT notification_deliveries_kind_check;

-- Deliveries of one channel are ordered per monitor; a channel waiting for its retry no longer
-- holds back the other channels of the same monitor. Same signature, owner and grants.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION claim_deliveries(n integer)
RETURNS TABLE (
    id           uuid,
    tenant_id    uuid,
    kind         text,
    monitor_id   uuid,
    incident_id  uuid,
    channel_id   uuid,
    payload      jsonb,
    attempts     integer,
    created_at   timestamptz,
    claim_token  uuid
)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    UPDATE notification_deliveries AS d
    SET status = CASE WHEN d.attempts + 1 >= 5 THEN 'failed' ELSE 'pending' END,
        attempts = d.attempts + 1,
        last_error = 'lease_expired',
        lease_until = NULL,
        claim_token = NULL
    WHERE d.id IN (
        SELECT e.id FROM notification_deliveries AS e
        WHERE e.status = 'in_progress' AND e.lease_until <= now()
        FOR UPDATE SKIP LOCKED
    );

    UPDATE notification_deliveries AS d
    SET status = 'failed', last_error = 'tenant_deleted'
    WHERE d.id IN (
        SELECT e.id FROM notification_deliveries AS e
        JOIN tenants AS t ON t.id = e.tenant_id
        WHERE e.status = 'pending' AND t.deleted_at IS NOT NULL
        FOR UPDATE OF e SKIP LOCKED
    );

    WITH ready AS (
        SELECT d.id
        FROM notification_deliveries AS d
        WHERE d.status = 'pending'
          AND d.next_attempt_at <= now()
          AND EXISTS (
              SELECT 1 FROM tenants AS t
              WHERE t.id = d.tenant_id AND t.status = 'active' AND t.deleted_at IS NULL
          )
          AND NOT EXISTS (
              SELECT 1
              FROM notification_deliveries AS o
              WHERE o.monitor_id = d.monitor_id
                AND o.channel_id = d.channel_id
                AND o.status IN ('pending', 'in_progress')
                AND (o.created_at, o.id) < (d.created_at, d.id)
          )
        ORDER BY d.created_at, d.id
        LIMIT least(greatest(n, 0), 100)
        FOR UPDATE OF d SKIP LOCKED
    )
    UPDATE notification_deliveries AS d
    SET status = 'in_progress', lease_until = now() + interval '2 minutes', claim_token = gen_random_uuid()
    FROM ready
    WHERE d.id = ready.id
    RETURNING d.id, d.tenant_id, d.kind, d.monitor_id, d.incident_id, d.channel_id, d.payload, d.attempts, d.created_at,
              d.claim_token
$$;
-- +goose StatementEnd

-- Deleting a channel (api, inside the tenant transaction that soft-deletes it) fails its pending
-- deliveries with last_error 'channel_deleted'. Only a channel of the caller's tenant that is
-- already deleted qualifies. Returns the number of deliveries failed.
-- +goose StatementBegin
CREATE FUNCTION fail_deleted_channel_deliveries(p_channel_id uuid)
RETURNS integer
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    WITH failed AS (
        UPDATE notification_deliveries AS d
        SET status = 'failed', last_error = 'channel_deleted'
        WHERE d.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
          AND d.channel_id = p_channel_id
          AND d.status = 'pending'
          AND EXISTS (
              SELECT 1 FROM notification_channels AS c
              WHERE c.tenant_id = d.tenant_id AND c.id = p_channel_id AND c.deleted_at IS NOT NULL
          )
        RETURNING 1
    )
    SELECT count(*)::integer FROM failed
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION fail_deleted_channel_deliveries(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fail_deleted_channel_deliveries(uuid) TO maiak_api;

-- Read and insert policies for enqueue_incident_reminders, which runs across tenants as the
-- owner (ADR-0021 pattern). monitors already has owner_claim_read.
CREATE POLICY owner_reminder_read ON incidents FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_reminder_read ON notification_channels FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_reminder_read ON monitor_channels FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_reminder_insert ON notification_deliveries FOR INSERT TO maiak_owner WITH CHECK (true);

-- Queues a 'reminder' delivery for every open incident and every channel that receives it (active,
-- not deleted, reminder_minutes > 0, event 'down', selected by the monitor or the monitor selects
-- none; the monitor notifies, is not paused or deleted; the tenant is active), once
-- reminder_minutes have passed since the newest 'opened' or 'reminder' delivery of that incident
-- to that channel (or since the incident started when there is none). At most p_limit rows per
-- call (capped at 1000), oldest incidents first. Delivery ids are UUIDv7 built from the clock.
-- jobs calls it every minute under an advisory lock. Returns the number of rows queued.
-- +goose StatementBegin
CREATE FUNCTION enqueue_incident_reminders(p_limit integer)
RETURNS integer
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
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
              EXISTS (SELECT 1 FROM monitor_channels AS mc
                      WHERE mc.tenant_id = i.tenant_id AND mc.monitor_id = m.id AND mc.channel_id = c.id)
              OR NOT EXISTS (SELECT 1 FROM monitor_channels AS mc
                             WHERE mc.tenant_id = i.tenant_id AND mc.monitor_id = m.id)
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
REVOKE ALL ON FUNCTION enqueue_incident_reminders(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enqueue_incident_reminders(integer) TO maiak_worker;
