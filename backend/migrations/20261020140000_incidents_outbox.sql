-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Incidents and the notification outbox (ADR-0007, ADR-0016). Owning module: internal/incidents
-- for incidents, incident_monitors and notification_deliveries; internal/notifications (B-13)
-- for notification_channels and monitor_channels.

-- An automatic incident covers one monitor; incident_monitors is many-to-many so that manual
-- incidents spanning monitors (status pages) need no schema change. reason is the error kind
-- that opened it; resolve_reason says why it ended.
CREATE TABLE incidents (
    id              uuid        PRIMARY KEY,
    tenant_id       uuid        NOT NULL REFERENCES tenants (id),
    started_at      timestamptz NOT NULL,
    resolved_at     timestamptz,
    resolve_reason  text        CHECK (resolve_reason IN ('recovered', 'paused', 'deleted', 'url_changed')),
    reason          text        CHECK (length(reason) BETWEEN 1 AND 64),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    CHECK ((resolved_at IS NULL) = (resolve_reason IS NULL))
);
CREATE INDEX incidents_tenant_started_idx ON incidents (tenant_id, started_at DESC);
-- The worker opens and resolves incidents; the api resolves them on pause and delete.
REVOKE DELETE ON incidents FROM maiak_api, maiak_worker;
REVOKE INSERT ON incidents FROM maiak_api;

CREATE TABLE incident_monitors (
    tenant_id    uuid NOT NULL,
    incident_id  uuid NOT NULL,
    monitor_id   uuid NOT NULL,
    PRIMARY KEY (incident_id, monitor_id),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES incidents (tenant_id, id),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
);
CREATE INDEX incident_monitors_monitor_idx ON incident_monitors (tenant_id, monitor_id);
REVOKE UPDATE, DELETE ON incident_monitors FROM maiak_api, maiak_worker;
REVOKE INSERT ON incident_monitors FROM maiak_api;

-- monitor_state.current_incident_id gets the foreign key promised in the monitors migration.
-- All values are NULL so far; NOT VALID + VALIDATE keeps the lock on monitor_state short.
ALTER TABLE monitor_state ADD CONSTRAINT monitor_state_current_incident_fkey
    FOREIGN KEY (tenant_id, current_incident_id) REFERENCES incidents (tenant_id, id) NOT VALID;
ALTER TABLE monitor_state VALIDATE CONSTRAINT monitor_state_current_incident_fkey;

-- Notification destinations. config_encrypted is the AES-256-GCM envelope (key id prefix) of
-- the channel configuration, for Slack the Incoming Webhook URL; it is never returned by the
-- api. One Slack channel per tenant in the MVP (ADR-0019). status 'failed' is set by jobs on a
-- permanent delivery failure (404/410).
CREATE TABLE notification_channels (
    id                uuid        PRIMARY KEY,
    tenant_id         uuid        NOT NULL REFERENCES tenants (id),
    type              text        NOT NULL CHECK (type IN ('slack_webhook')),
    config_encrypted  bytea       NOT NULL CHECK (length(config_encrypted) BETWEEN 1 AND 4096),
    status            text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'failed')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX notification_channels_one_slack_idx ON notification_channels (tenant_id) WHERE type = 'slack_webhook';
REVOKE DELETE ON notification_channels FROM maiak_api, maiak_worker;
REVOKE INSERT, UPDATE ON notification_channels FROM maiak_worker;
GRANT UPDATE (status, updated_at) ON notification_channels TO maiak_worker;

-- Which channels notify for which monitor. Unused by the MVP delivery path (ADR-0019).
CREATE TABLE monitor_channels (
    tenant_id   uuid        NOT NULL,
    monitor_id  uuid        NOT NULL,
    channel_id  uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (monitor_id, channel_id),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES notification_channels (tenant_id, id)
);
CREATE INDEX monitor_channels_channel_idx ON monitor_channels (tenant_id, channel_id);
REVOKE UPDATE ON monitor_channels FROM maiak_api;
REVOKE INSERT, UPDATE, DELETE ON monitor_channels FROM maiak_worker;

-- The transactional outbox: rows are inserted in the transaction of the state change and
-- delivered by jobs through claim_deliveries / finish_delivery. payload holds what the message
-- needs and never a secret (the webhook URL stays in the channel). lease_until is set while a
-- delivery is in progress, so rows of a dead jobs process go back to pending.
CREATE TABLE notification_deliveries (
    id               uuid        PRIMARY KEY,
    tenant_id        uuid        NOT NULL,
    kind             text        NOT NULL CHECK (kind IN ('opened', 'resolved', 'tls_expiring')),
    monitor_id       uuid        NOT NULL,
    incident_id      uuid,
    channel_id       uuid        NOT NULL,
    payload          jsonb       NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    status           text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'in_progress', 'sent', 'failed')),
    attempts         integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    lease_until      timestamptz,
    last_error       text        CHECK (length(last_error) <= 500),
    created_at       timestamptz NOT NULL DEFAULT now(),
    sent_at          timestamptz,
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES incidents (tenant_id, id),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES notification_channels (tenant_id, id),
    CHECK ((status = 'in_progress') = (lease_until IS NOT NULL)),
    CHECK ((status = 'sent') = (sent_at IS NOT NULL)),
    CHECK (kind = 'tls_expiring' OR incident_id IS NOT NULL)
);
CREATE INDEX notification_deliveries_ready_idx ON notification_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX notification_deliveries_lease_idx ON notification_deliveries (lease_until) WHERE status = 'in_progress';
-- "Oldest undelivered row of this monitor" for the per-monitor ordering in claim_deliveries.
CREATE INDEX notification_deliveries_monitor_open_idx ON notification_deliveries (monitor_id, created_at, id)
    WHERE status IN ('pending', 'in_progress');
CREATE INDEX notification_deliveries_tenant_created_idx ON notification_deliveries (tenant_id, created_at DESC);
-- The worker inserts; status changes only through finish_delivery.
REVOKE INSERT, UPDATE, DELETE ON notification_deliveries FROM maiak_api;
REVOKE UPDATE, DELETE ON notification_deliveries FROM maiak_worker;

ALTER TABLE incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE incidents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON incidents
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE incident_monitors ENABLE ROW LEVEL SECURITY;
ALTER TABLE incident_monitors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON incident_monitors
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE notification_channels ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_channels FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_channels
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE monitor_channels ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitor_channels FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitor_channels
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE notification_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_deliveries
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- For the definer functions below (ADR-0021 pattern).
CREATE POLICY owner_delivery_read ON notification_deliveries FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_delivery_update ON notification_deliveries FOR UPDATE TO maiak_owner USING (true) WITH CHECK (true);

-- Claims up to n deliveries across tenants, oldest first. Only the oldest undelivered row
-- (pending or in progress) of each monitor is eligible, so a monitor never has two deliveries in
-- flight and opened/resolved arrive in order; a row waiting for its retry blocks the newer ones
-- of the same monitor. Rows left in progress by a dead jobs process return to pending when
-- their 2-minute lease expires. Claimed rows are marked in_progress with a new lease.
-- +goose StatementBegin
CREATE FUNCTION claim_deliveries(n integer)
RETURNS TABLE (
    id           uuid,
    tenant_id    uuid,
    kind         text,
    monitor_id   uuid,
    incident_id  uuid,
    channel_id   uuid,
    payload      jsonb,
    attempts     integer,
    created_at   timestamptz
)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    UPDATE notification_deliveries AS d
    SET status = 'pending', lease_until = NULL
    WHERE d.id IN (
        SELECT e.id FROM notification_deliveries AS e
        WHERE e.status = 'in_progress' AND e.lease_until <= now()
        FOR UPDATE SKIP LOCKED
    );

    WITH ready AS (
        SELECT d.id
        FROM notification_deliveries AS d
        WHERE d.status = 'pending'
          AND d.next_attempt_at <= now()
          AND NOT EXISTS (
              SELECT 1
              FROM notification_deliveries AS o
              WHERE o.monitor_id = d.monitor_id
                AND o.status IN ('pending', 'in_progress')
                AND (o.created_at, o.id) < (d.created_at, d.id)
          )
        ORDER BY d.created_at, d.id
        LIMIT least(greatest(n, 0), 100)
        FOR UPDATE OF d SKIP LOCKED
    )
    UPDATE notification_deliveries AS d
    SET status = 'in_progress', lease_until = now() + interval '2 minutes'
    FROM ready
    WHERE d.id = ready.id
    RETURNING d.id, d.tenant_id, d.kind, d.monitor_id, d.incident_id, d.channel_id, d.payload, d.attempts, d.created_at
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION claim_deliveries(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_deliveries(integer) TO maiak_worker;

-- Records the outcome of a claimed delivery: 'sent', 'failed' (permanent) or 'pending' (retry
-- at retry_at). Counts the attempt and ends the lease. Returns false when the row is not in
-- progress (already finished, or its lease expired and it was requeued).
-- +goose StatementBegin
CREATE FUNCTION finish_delivery(p_id uuid, p_status text, p_error text, p_retry_at timestamptz)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF p_status IS NULL OR p_status NOT IN ('sent', 'failed', 'pending') THEN
        RAISE EXCEPTION 'finish_delivery: invalid status' USING ERRCODE = '22023';
    END IF;
    IF p_status = 'pending' AND p_retry_at IS NULL THEN
        RAISE EXCEPTION 'finish_delivery: retry_at is required for pending' USING ERRCODE = '22023';
    END IF;
    UPDATE notification_deliveries AS d
    SET status = p_status,
        attempts = d.attempts + 1,
        lease_until = NULL,
        last_error = CASE WHEN p_status = 'sent' THEN NULL ELSE left(p_error, 500) END,
        sent_at = CASE WHEN p_status = 'sent' THEN now() END,
        next_attempt_at = CASE WHEN p_status = 'pending' THEN p_retry_at ELSE d.next_attempt_at END
    WHERE d.id = p_id AND d.status = 'in_progress';
    RETURN FOUND;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION finish_delivery(uuid, text, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION finish_delivery(uuid, text, text, timestamptz) TO maiak_worker;
