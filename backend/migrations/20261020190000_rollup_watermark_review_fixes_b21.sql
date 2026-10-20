-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Rollup catch-up (B-21, ADR-0090). compute_rollups(from, to) only recomputed the last 3 closed
-- hours on every jobs run, so hours missed during a jobs outage longer than that were never rolled
-- up, and retention then kept their partition forever. jobs now calls compute_rollups_catch_up,
-- which remembers how far it got.
--
-- rollup_state holds that watermark: one row (singleton = true), owned by maiak_owner and
-- written only by compute_rollups_catch_up. It is not tenant data (one timestamp for the whole
-- system), so it has no tenant_id and is outside the tenant RLS rules of ADR-0004; the schema
-- lint only applies them to tables with a tenant_id. The runtime roles get no privilege on it (the
-- default privileges of the first migration would grant them DML, revoked below), and RLS is
-- enabled without a policy, so even a stray future grant shows them no row. The owner is not
-- subject to non-forced RLS, which is what the definer function needs.
CREATE TABLE rollup_state (
    singleton   boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    -- Every closed UTC hour before the watermark has been rolled up once.
    watermark   timestamptz NOT NULL CHECK (watermark = date_trunc('hour', watermark, 'UTC')),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
REVOKE ALL ON rollup_state FROM PUBLIC, maiak_api, maiak_worker;
ALTER TABLE rollup_state ENABLE ROW LEVEL SECURITY;

-- Rolls up the closed UTC hours from the watermark onwards, at most p_max_hours of them, and
-- moves the watermark to the end of what it covered, so a long outage is caught up over several
-- runs. On the first run (no watermark yet) it starts at the earliest raw hour of the last 7
-- days. The watermark is never older than 7 days: hours before that are left to a manual
-- compute_rollups(from, to) run (L-130). Every run also recomputes the last p_lookback_hours
-- closed hours, as the previous lookback did, so late results are absorbed. The upsert is
-- compute_rollups(from, to) itself, so idempotency, the advisory lock that serialises concurrent
-- calls and the open-hour cap are unchanged; the lock is taken before the watermark is read.
-- compute_rollups(from, to) stays for the previous jobs release and for manual backfills
-- (expand/contract). Returns the number of rollup rows written.
-- +goose StatementBegin
CREATE FUNCTION compute_rollups_catch_up(p_max_hours integer, p_lookback_hours integer)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
SET TimeZone = 'UTC'
AS $$
DECLARE
    closed_end timestamptz := date_trunc('hour', now());
    floor_hour timestamptz := date_trunc('hour', now()) - interval '7 days';
    w          timestamptz;
    lo         timestamptz;
    hi         timestamptz;
    written    integer;
BEGIN
    IF p_max_hours IS NULL OR p_max_hours NOT BETWEEN 1 AND 168
       OR p_lookback_hours IS NULL OR p_lookback_hours NOT BETWEEN 0 AND 24 THEN
        RAISE EXCEPTION 'compute_rollups_catch_up: p_max_hours must be 1..168 and p_lookback_hours 0..24'
            USING ERRCODE = '22023';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('maiak.compute_rollups'));
    SELECT s.watermark INTO w FROM rollup_state AS s WHERE s.singleton;
    IF w IS NULL THEN
        SELECT date_trunc('hour', min(c.checked_at)) INTO w
        FROM check_results AS c
        WHERE c.checked_at >= floor_hour AND c.checked_at < closed_end;
        w := COALESCE(w, closed_end);
    END IF;
    w  := least(greatest(w, floor_hour), closed_end);
    hi := least(w + make_interval(hours => p_max_hours), closed_end);
    lo := least(w, closed_end - make_interval(hours => p_lookback_hours));
    written := compute_rollups(lo, hi);
    INSERT INTO rollup_state AS s (singleton, watermark, updated_at) VALUES (true, hi, now())
    ON CONFLICT (singleton) DO UPDATE SET watermark = EXCLUDED.watermark, updated_at = EXCLUDED.updated_at;
    RETURN written;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION compute_rollups_catch_up(integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION compute_rollups_catch_up(integer, integer) TO maiak_worker;

-- reset_monitor_runtime (ADR-0018) also refuses soft-deleted monitors. The api never calls it for
-- one (the monitor service returns 404 first); this is defence in depth, so a deleted monitor's
-- leftover state rows cannot be made due again. CREATE OR REPLACE keeps the owner and the grants
-- (EXECUTE for maiak_api only); search_path is pinned again by the definition.
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
        next_run_at = now() + random() * interval '30 seconds'
    FROM monitors AS m
    WHERE m.tenant_id = caller_tenant AND m.id = p_monitor_id AND m.deleted_at IS NULL
      AND s.tenant_id = m.tenant_id AND s.monitor_id = m.id;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;
-- +goose StatementEnd
