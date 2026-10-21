-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Heartbeat (watchdog) monitors (P-03, ADR-0094). A heartbeat is a monitor of type 'heartbeat'
-- in the same tables as http monitors: it has no target, the client pings a secret URL, and a
-- missed deadline (last ping or start of the wait + interval + grace) is a failed result in the
-- incident machine. Expand only: the previous app version never creates heartbeats, and the
-- claim function below (same signature) never hands one to a worker of any version.

-- The MVP constraints (60 s interval, 10 s timeout) apply to http monitors only. A heartbeat
-- stores its expected interval in interval_seconds, its grace in config.graceSeconds and always
-- has failure_threshold 1 (one missed deadline is down); timeout_seconds keeps the default 10.
ALTER TABLE monitors DROP CONSTRAINT monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check CHECK (type IN ('http', 'heartbeat'));
ALTER TABLE monitors DROP CONSTRAINT monitors_mvp_interval;
ALTER TABLE monitors ADD CONSTRAINT monitors_mvp_interval CHECK (type <> 'http' OR interval_seconds = 60);
ALTER TABLE monitors DROP CONSTRAINT monitors_mvp_timeout;
ALTER TABLE monitors ADD CONSTRAINT monitors_mvp_timeout CHECK (type <> 'http' OR timeout_seconds = 10);
-- CASE guards the cast: a CHECK does not promise to evaluate AND operands left to right.
ALTER TABLE monitors ADD CONSTRAINT monitors_heartbeat_check CHECK (
    type <> 'heartbeat' OR (
        interval_seconds BETWEEN 60 AND 86400
        AND failure_threshold = 1
        AND CASE WHEN jsonb_typeof(config -> 'graceSeconds') = 'number'
                 THEN (config ->> 'graceSeconds')::numeric BETWEEN 0 AND 3600
                      AND (config ->> 'graceSeconds')::numeric = trunc((config ->> 'graceSeconds')::numeric)
                 ELSE false END
    )
);

-- One row per heartbeat monitor. Owning module: internal/monitors. Only the SHA-256 of the token
-- is stored; the token is shown once (create, regenerate) and cannot be shown again.
--   waiting_since   start of the current wait: creation or the last resume
--   last_ping_at    newest accepted ping (written by heartbeat_ping only)
--   applied_ping_at newest ping the incident machine has seen (jobs)
--   missed_deadline newest deadline already applied as missed (jobs), so one deadline opens at
--                   most one failure
-- The deadline is greatest(waiting_since, last_ping_at) + interval_seconds + graceSeconds.
CREATE TABLE heartbeat_tokens (
    tenant_id        uuid        NOT NULL,
    monitor_id       uuid        PRIMARY KEY,
    token_hash       bytea       NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at       timestamptz NOT NULL DEFAULT now(),
    waiting_since    timestamptz NOT NULL DEFAULT now(),
    last_ping_at     timestamptz,
    applied_ping_at  timestamptz,
    missed_deadline  timestamptz,
    UNIQUE (tenant_id, monitor_id),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
);
ALTER TABLE heartbeat_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE heartbeat_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON heartbeat_tokens
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
-- heartbeat_ping and list_due_heartbeats run as maiak_owner before the tenant is known (same
-- pattern as owner_claim_read, ADR-0016).
CREATE POLICY owner_heartbeat_read ON heartbeat_tokens FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_heartbeat_ping ON heartbeat_tokens FOR UPDATE TO maiak_owner USING (true) WITH CHECK (true);

-- The api creates rows, replaces the token, restarts the wait on resume and locks the row; it
-- never writes the ping or the evaluation columns. jobs (maiak_worker) records what it applied.
REVOKE UPDATE, DELETE ON heartbeat_tokens FROM maiak_api;
GRANT UPDATE (token_hash, created_at, waiting_since, applied_ping_at) ON heartbeat_tokens TO maiak_api;
REVOKE ALL ON heartbeat_tokens FROM maiak_worker;
GRANT SELECT ON heartbeat_tokens TO maiak_worker;
GRANT UPDATE (applied_ping_at, missed_deadline) ON heartbeat_tokens TO maiak_worker;

-- A ping: the public endpoint calls this with the SHA-256 of the token from the URL. For a live
-- heartbeat (not paused, not deleted, tenant active) it records the ping time; it returns the
-- tenant and monitor of any known token with recorded = false when nothing was recorded, and no
-- row for an unknown token. It reveals nothing else and accepts only an exact 32-byte hash, so
-- it cannot enumerate tokens. EXECUTE for maiak_api only.
-- +goose StatementBegin
CREATE FUNCTION heartbeat_ping(p_token_hash bytea)
RETURNS TABLE (tenant_id uuid, monitor_id uuid, recorded boolean)
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    v_tenant  uuid;
    v_monitor uuid;
BEGIN
    IF p_token_hash IS NULL OR length(p_token_hash) <> 32 THEN
        RETURN;
    END IF;
    UPDATE heartbeat_tokens AS h
    SET last_ping_at = now()
    FROM monitors AS m, tenants AS t
    WHERE h.token_hash = p_token_hash
      AND m.tenant_id = h.tenant_id AND m.id = h.monitor_id
      AND NOT m.paused AND m.deleted_at IS NULL
      AND t.id = h.tenant_id AND t.status = 'active' AND t.deleted_at IS NULL
    RETURNING h.tenant_id, h.monitor_id INTO v_tenant, v_monitor;
    IF FOUND THEN
        RETURN QUERY SELECT v_tenant, v_monitor, true;
        RETURN;
    END IF;
    RETURN QUERY
        SELECT h.tenant_id, h.monitor_id, false
        FROM heartbeat_tokens AS h
        WHERE h.token_hash = p_token_hash;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION heartbeat_ping(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION heartbeat_ping(bytea) TO maiak_api;

-- The heartbeats jobs has to look at: live ones with a ping the incident machine has not seen, or
-- whose deadline has passed and was not applied as missed yet. Read only: jobs runs it under its
-- singleton advisory lock and re-checks each row under a row lock in the tenant transaction,
-- where it also records what it applied, so a crash in between loses nothing. EXECUTE for
-- maiak_worker only.
-- +goose StatementBegin
CREATE FUNCTION list_due_heartbeats(n integer)
RETURNS TABLE (tenant_id uuid, monitor_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT h.tenant_id, h.monitor_id
    FROM heartbeat_tokens AS h
    JOIN monitors AS m ON m.tenant_id = h.tenant_id AND m.id = h.monitor_id
    JOIN tenants AS t ON t.id = h.tenant_id
    CROSS JOIN LATERAL (
        SELECT greatest(h.waiting_since, coalesce(h.last_ping_at, h.waiting_since))
               + make_interval(secs => m.interval_seconds
                   + CASE WHEN m.type = 'heartbeat' THEN (m.config ->> 'graceSeconds')::integer ELSE 0 END) AS deadline
    ) AS d
    WHERE m.type = 'heartbeat'
      AND NOT m.paused
      AND m.deleted_at IS NULL
      AND t.status = 'active'
      AND t.deleted_at IS NULL
      AND ((h.last_ping_at IS NOT NULL AND (h.applied_ping_at IS NULL OR h.last_ping_at > h.applied_ping_at))
           OR (d.deadline < now() AND (h.missed_deadline IS NULL OR h.missed_deadline < d.deadline)))
    ORDER BY d.deadline, h.monitor_id
    LIMIT least(greatest(n, 0), 1000)
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION list_due_heartbeats(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION list_due_heartbeats(integer) TO maiak_worker;

-- Workers check http monitors only. The body changes, the signature does not, so workers of the
-- previous release keep working and never receive a heartbeat either. CREATE OR REPLACE keeps the
-- owner and the grants (EXECUTE for maiak_worker only).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION claim_due_monitors(n integer, p_location text)
RETURNS TABLE (
    tenant_id          uuid,
    monitor_id         uuid,
    location_id        uuid,
    lease_token        uuid,
    lease_until        timestamptz,
    url                text,
    expected_status    text,
    failure_threshold  integer,
    interval_seconds   integer,
    timeout_seconds    integer,
    next_run_at        timestamptz
)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    WITH due AS (
        SELECT s.monitor_id, s.location_id
        FROM monitor_location_state AS s
        JOIN monitors AS m ON m.tenant_id = s.tenant_id AND m.id = s.monitor_id
        JOIN tenants AS t ON t.id = s.tenant_id
        JOIN probe_locations AS l ON l.id = s.location_id
        WHERE l.code = p_location
          AND l.status = 'active'
          AND t.status = 'active'
          AND t.deleted_at IS NULL
          AND s.next_run_at <= now()
          AND (s.lease_token IS NULL OR s.lease_until <= now())
          AND m.type = 'http'
          AND NOT m.paused
          AND m.deleted_at IS NULL
        ORDER BY s.next_run_at
        LIMIT least(greatest(n, 0), 1000)
        FOR UPDATE OF s SKIP LOCKED
    )
    UPDATE monitor_location_state AS s
    SET lease_token = gen_random_uuid(),
        lease_until = now() + interval '30 seconds'
    FROM due, monitors AS m
    WHERE s.monitor_id = due.monitor_id AND s.location_id = due.location_id
      AND m.tenant_id = s.tenant_id AND m.id = s.monitor_id
    RETURNING s.tenant_id, s.monitor_id, s.location_id, s.lease_token, s.lease_until,
              m.config ->> 'url', COALESCE(m.config ->> 'expectedStatus', '2xx'),
              m.failure_threshold, m.interval_seconds, m.timeout_seconds, s.next_run_at
$$;
-- +goose StatementEnd

-- A heartbeat's schedule row exists only for the incident machine's per-location state; it is
-- parked at next_run_at = 'infinity' when created, and the reset on pause and resume keeps it
-- there, so it never sits in the due index. Same signature, owner and grants (EXECUTE for
-- maiak_api only).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION reset_monitor_runtime(p_monitor_id uuid)
RETURNS integer
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    caller_tenant uuid := NULLIF(current_setting('app.tenant_id', true), '')::uuid;
    n             integer;
BEGIN
    IF caller_tenant IS NULL THEN
        RAISE EXCEPTION 'reset_monitor_runtime: app.tenant_id is not set' USING ERRCODE = 'insufficient_privilege';
    END IF;
    UPDATE monitor_location_state AS s
    SET status = 'unknown',
        consecutive_failures = 0,
        lease_token = NULL,
        lease_until = NULL,
        next_run_at = CASE WHEN m.type = 'http' THEN now() + random() * interval '30 seconds'
                           ELSE 'infinity'::timestamptz END
    FROM monitors AS m
    WHERE m.tenant_id = caller_tenant AND m.id = p_monitor_id AND m.deleted_at IS NULL
      AND s.tenant_id = m.tenant_id AND s.monitor_id = m.id;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;
-- +goose StatementEnd
