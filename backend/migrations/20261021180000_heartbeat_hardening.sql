-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Review fixes for heartbeats (P-08, ADR-0096). Expand only: the previous app version writes
-- graceSeconds as a JSON integer, which the stricter check accepts, and both functions keep their
-- signatures, owners and grants (CREATE OR REPLACE).

-- graceSeconds must be written as a plain non-negative integer (`^[0-9]+$` as text), so every
-- cast of it to integer succeeds: the previous check accepted 60.0, whose text '60.0' fails
-- `::integer` in list_due_heartbeats and the dashboard query, and fails json.Unmarshal into an
-- int. The expected interval lives in the integer column interval_seconds and needs no pattern.
-- Rows the previous check accepted with an integral non-integer spelling (60.0) are rewritten
-- to the integer; anything else fails the migration loudly with the monitor ids.
-- monitors forces RLS even for its owner, so the rewrite runs per tenant under app.tenant_id;
-- owner_claim_read lets the owner find the rows across tenants.
-- +goose StatementBegin
DO $$
DECLARE
    v_tenant uuid;
BEGIN
    FOR v_tenant IN
        SELECT DISTINCT tenant_id FROM monitors
        WHERE type = 'heartbeat'
          AND jsonb_typeof(config -> 'graceSeconds') = 'number'
          AND (config ->> 'graceSeconds') !~ '^[0-9]+$'
    LOOP
        PERFORM set_config('app.tenant_id', v_tenant::text, true);
        UPDATE monitors
        SET config = jsonb_set(config, '{graceSeconds}', to_jsonb((config ->> 'graceSeconds')::numeric::integer))
        WHERE tenant_id = v_tenant
          AND type = 'heartbeat'
          AND CASE WHEN jsonb_typeof(config -> 'graceSeconds') = 'number' AND (config ->> 'graceSeconds') !~ '^[0-9]+$'
                   THEN (config ->> 'graceSeconds')::numeric BETWEEN 0 AND 3600
                        AND (config ->> 'graceSeconds')::numeric = trunc((config ->> 'graceSeconds')::numeric)
                   ELSE false END;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    bad text;
BEGIN
    SELECT string_agg(id::text, ', ' ORDER BY id) INTO bad
    FROM monitors
    WHERE type = 'heartbeat'
      AND CASE WHEN jsonb_typeof(config -> 'graceSeconds') = 'number' AND (config ->> 'graceSeconds') ~ '^[0-9]+$'
               THEN (config ->> 'graceSeconds')::numeric BETWEEN 0 AND 3600
                    AND interval_seconds BETWEEN 60 AND 86400
                    AND failure_threshold = 1
               ELSE false END IS NOT TRUE;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'heartbeat monitors with an invalid configuration (fix them, then rerun): %', bad
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;
-- +goose StatementEnd

-- CASE guards the casts here and above: SQL does not promise to evaluate AND operands left to right.
ALTER TABLE monitors DROP CONSTRAINT monitors_heartbeat_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_heartbeat_check CHECK (
    type <> 'heartbeat' OR (
        interval_seconds BETWEEN 60 AND 86400
        AND failure_threshold = 1
        AND CASE WHEN jsonb_typeof(config -> 'graceSeconds') = 'number'
                      AND (config ->> 'graceSeconds') ~ '^[0-9]+$'
                 THEN (config ->> 'graceSeconds')::numeric BETWEEN 0 AND 3600
                 ELSE false END
    )
);

-- A ping first takes the row lock, then reads the clock: a ping that waited for an evaluation
-- (which holds the lock and decides with clock_timestamp() read under it) records a time after
-- that evaluation, never one from before the wait. greatest() keeps last_ping_at from moving
-- backwards. Same signature, owner and grants (EXECUTE for maiak_api only).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION heartbeat_ping(p_token_hash bytea)
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
    PERFORM 1 FROM heartbeat_tokens AS h WHERE h.token_hash = p_token_hash FOR UPDATE;
    UPDATE heartbeat_tokens AS h
    SET last_ping_at = greatest(h.last_ping_at, clock_timestamp())
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

-- Same body as before with the grace cast through numeric (defence in depth: the check above
-- already guarantees an integer). Same signature, owner and grants (EXECUTE for maiak_worker only).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION list_due_heartbeats(n integer)
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
                   + CASE WHEN m.type = 'heartbeat' THEN (m.config ->> 'graceSeconds')::numeric::integer ELSE 0 END) AS deadline
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
