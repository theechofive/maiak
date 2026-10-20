-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Fixes from the review of B-10..B-12 (ADR-0022, ADR-0060).
--
-- Expand/contract note (ledger L-090): claim_due_monitors(integer) and
-- finish_delivery(uuid, text, text, timestamptz) are replaced by new signatures, and
-- drop_expired_partitions() by the list/detach/drop functions below. Between this migration and
-- the end of the rollout a worker or jobs pod of the previous release fails those calls: claims
-- and housekeeping are retried by the new pods, a delivery it finished is not recorded, its lease
-- expires and the row is delivered again (at-least-once, ADR-0007).

-- At most one open automatic incident per monitor. incidents had no monitor column (the link is
-- incident_monitors, many-to-many for manual incidents); an automatic incident now also records
-- its monitor so the database can enforce the invariant. Manual incidents leave it NULL.
ALTER TABLE incidents ADD COLUMN monitor_id uuid;
ALTER TABLE incidents ADD CONSTRAINT incidents_monitor_fkey
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id) NOT VALID;
-- The backfill spans tenants; the owner is subject to FORCE ROW LEVEL SECURITY, so it is lifted
-- for this transaction only (DDL is transactional: no other session sees it lifted).
ALTER TABLE incidents NO FORCE ROW LEVEL SECURITY;
ALTER TABLE incident_monitors NO FORCE ROW LEVEL SECURITY;
UPDATE incidents AS i
SET monitor_id = im.monitor_id
FROM incident_monitors AS im
WHERE im.incident_id = i.id
  AND (SELECT count(*) FROM incident_monitors AS o WHERE o.incident_id = i.id) = 1;
ALTER TABLE incidents FORCE ROW LEVEL SECURITY;
ALTER TABLE incident_monitors FORCE ROW LEVEL SECURITY;
ALTER TABLE incidents VALIDATE CONSTRAINT incidents_monitor_fkey;
CREATE UNIQUE INDEX incidents_one_open_per_monitor_idx ON incidents (monitor_id)
    WHERE resolved_at IS NULL AND monitor_id IS NOT NULL;

-- claim_due_monitors now claims only for one location and skips tenants that are suspended or
-- deleted. tenants and probe_locations have no RLS, the owner reads them directly. Monitors of
-- such a tenant keep their schedule and run again once the tenant is active.
DROP FUNCTION claim_due_monitors(integer);
-- +goose StatementBegin
CREATE FUNCTION claim_due_monitors(n integer, p_location text)
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
REVOKE ALL ON FUNCTION claim_due_monitors(integer, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_due_monitors(integer, text) TO maiak_worker;

-- Outbox claims are fenced by a token: finish_delivery must present the token of the claim it
-- finishes, so a jobs process whose lease expired (and whose row was claimed again) cannot
-- record a second outcome. Rows in progress at migration time get a token nobody holds; their
-- lease expires and they are requeued.
ALTER TABLE notification_deliveries ADD COLUMN claim_token uuid;
UPDATE notification_deliveries SET claim_token = gen_random_uuid() WHERE status = 'in_progress';
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_claim_token_check
    CHECK ((status = 'in_progress') = (claim_token IS NOT NULL));

-- The requeue of an expired claim counts as an attempt, so a message that kills or hangs every
-- jobs process (a poison message) ends as failed instead of being claimed forever. 5 is
-- jobs.MaxAttempts; the jobs integration test fails if they drift apart.
DROP FUNCTION claim_deliveries(integer);
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
    SET status = 'in_progress', lease_until = now() + interval '2 minutes', claim_token = gen_random_uuid()
    FROM ready
    WHERE d.id = ready.id
    RETURNING d.id, d.tenant_id, d.kind, d.monitor_id, d.incident_id, d.channel_id, d.payload, d.attempts, d.created_at,
              d.claim_token
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION claim_deliveries(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_deliveries(integer) TO maiak_worker;

DROP FUNCTION finish_delivery(uuid, text, text, timestamptz);
-- Records the outcome of a claimed delivery: 'sent', 'failed' (permanent) or 'pending' (retry
-- at retry_at). Counts the attempt and ends the lease. Returns false when the row is not in
-- progress under p_token (already finished, or requeued and possibly claimed again).
-- +goose StatementBegin
CREATE FUNCTION finish_delivery(p_id uuid, p_token uuid, p_status text, p_error text, p_retry_at timestamptz)
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
        claim_token = NULL,
        last_error = CASE WHEN p_status = 'sent' THEN NULL ELSE left(p_error, 500) END,
        sent_at = CASE WHEN p_status = 'sent' THEN now() END,
        next_attempt_at = CASE WHEN p_status = 'pending' THEN p_retry_at ELSE d.next_attempt_at END
    WHERE d.id = p_id AND d.status = 'in_progress' AND d.claim_token = p_token;
    RETURN FOUND;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION finish_delivery(uuid, uuid, text, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION finish_delivery(uuid, uuid, text, text, timestamptz) TO maiak_worker;

-- compute_rollups: two concurrent calls (two jobs replicas, or a manual run) are serialised by a
-- transaction advisory lock, so an older snapshot never overwrites a newer upsert. The lock is
-- taken before the INSERT, whose snapshot is therefore taken after the other call committed.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION compute_rollups(from_hour timestamptz, to_hour timestamptz)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
SET TimeZone = 'UTC'
AS $$
DECLARE
    lo      timestamptz := date_trunc('hour', from_hour);
    hi      timestamptz := least(date_trunc('hour', to_hour), date_trunc('hour', now()));
    written integer;
BEGIN
    IF lo IS NULL OR hi IS NULL OR lo >= hi THEN
        RETURN 0;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('maiak.compute_rollups'));
    INSERT INTO result_rollups_hourly AS r
        (tenant_id, monitor_id, location_id, hour, total, ok_count, latency_sum_ms, latency_histogram, computed_at)
    SELECT c.tenant_id, c.monitor_id, c.location_id, date_trunc('hour', c.checked_at),
           count(*), count(*) FILTER (WHERE c.ok), COALESCE(sum(c.latency_ms), 0),
           ARRAY[
               count(*) FILTER (WHERE c.latency_ms <= 10),
               count(*) FILTER (WHERE c.latency_ms > 10 AND c.latency_ms <= 25),
               count(*) FILTER (WHERE c.latency_ms > 25 AND c.latency_ms <= 50),
               count(*) FILTER (WHERE c.latency_ms > 50 AND c.latency_ms <= 100),
               count(*) FILTER (WHERE c.latency_ms > 100 AND c.latency_ms <= 250),
               count(*) FILTER (WHERE c.latency_ms > 250 AND c.latency_ms <= 500),
               count(*) FILTER (WHERE c.latency_ms > 500 AND c.latency_ms <= 1000),
               count(*) FILTER (WHERE c.latency_ms > 1000 AND c.latency_ms <= 2500),
               count(*) FILTER (WHERE c.latency_ms > 2500 AND c.latency_ms <= 5000),
               count(*) FILTER (WHERE c.latency_ms > 5000 AND c.latency_ms <= 10000),
               count(*) FILTER (WHERE c.latency_ms > 10000)
           ]::integer[],
           now()
    FROM check_results AS c
    WHERE c.checked_at >= lo AND c.checked_at < hi
    GROUP BY c.tenant_id, c.monitor_id, c.location_id, date_trunc('hour', c.checked_at)
    ON CONFLICT (monitor_id, location_id, hour) DO UPDATE
    SET total = EXCLUDED.total,
        ok_count = EXCLUDED.ok_count,
        latency_sum_ms = EXCLUDED.latency_sum_ms,
        latency_histogram = EXCLUDED.latency_histogram,
        computed_at = EXCLUDED.computed_at;
    GET DIAGNOSTICS written = ROW_COUNT;
    RETURN written;
END
$$;
-- +goose StatementEnd

-- Retention in three steps, each its own transaction driven by jobs (ADR-0060): list the
-- candidates, detach one, drop it. DETACH PARTITION ... CONCURRENTLY cannot run inside a
-- function or a transaction block, and maiak_worker does not own check_results, so the detach
-- is a plain DETACH inside a definer function under a short lock_timeout: the ACCESS EXCLUSIVE
-- lock on the parent is held only for the catalog change, and a busy parent makes the call fail
-- fast (it is retried) instead of queueing inserts behind it. The DROP then touches only the
-- detached table, never the parent.
DROP FUNCTION drop_expired_partitions();

-- Partitions named check_results_pYYYYMM whose month ended more than 30 days ago (never the
-- current month), attached or already detached by an earlier, interrupted run. Catalog only:
-- rollup completeness is checked by detach_expired_partition.
-- +goose StatementBegin
CREATE FUNCTION list_droppable_partitions()
RETURNS TABLE (name text, detached boolean)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
SET TimeZone = 'UTC'
AS $$
    SELECT c.relname::text, NOT c.relispartition
    FROM pg_class AS c
    JOIN pg_namespace AS ns ON ns.oid = c.relnamespace AND ns.nspname = 'public'
    WHERE c.relkind = 'r'
      AND c.relname ~ '^check_results_p[0-9]{4}(0[1-9]|1[0-2])$'
      AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
      AND (NOT c.relispartition
           OR EXISTS (SELECT 1 FROM pg_inherits AS i WHERE i.inhrelid = c.oid AND i.inhparent = 'public.check_results'::regclass))
      AND make_timestamptz(substr(c.relname, 16, 4)::integer, substr(c.relname, 20, 2)::integer, 1, 0, 0, 0, 'UTC')
          + interval '1 month' < now() - interval '30 days'
      AND make_timestamptz(substr(c.relname, 16, 4)::integer, substr(c.relname, 20, 2)::integer, 1, 0, 0, 0, 'UTC')
          <> date_trunc('month', now())
    ORDER BY 1
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION list_droppable_partitions() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION list_droppable_partitions() TO maiak_worker;

-- Detaches one expired partition after checking that its rollups are complete (every
-- (monitor, location, hour) with raw rows has a rollup with the same total; ADR-0021). Returns
-- false when the partition is kept (incomplete rollups, or nothing to detach). The name is
-- validated against the naming pattern and must be an attached child of check_results owned by
-- maiak_owner; it reaches the DDL only through format('%I').
-- +goose StatementBegin
CREATE FUNCTION detach_expired_partition(p_name text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
SET TimeZone = 'UTC'
SET lock_timeout = '1s'
AS $$
DECLARE
    lo         timestamptz;
    hi         timestamptz;
    incomplete boolean;
BEGIN
    IF p_name IS NULL OR p_name !~ '^check_results_p[0-9]{4}(0[1-9]|1[0-2])$' THEN
        RAISE EXCEPTION 'detach_expired_partition: invalid partition name' USING ERRCODE = '22023';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('maiak.ensure_partitions'));
    IF NOT EXISTS (
        SELECT 1
        FROM pg_inherits AS i
        JOIN pg_class AS c ON c.oid = i.inhrelid
        JOIN pg_namespace AS ns ON ns.oid = c.relnamespace AND ns.nspname = 'public'
        WHERE i.inhparent = 'public.check_results'::regclass
          AND c.relname = p_name
          AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
    ) THEN
        RETURN false;
    END IF;
    lo := make_timestamptz(substr(p_name, 16, 4)::integer, substr(p_name, 20, 2)::integer, 1, 0, 0, 0, 'UTC');
    hi := lo + interval '1 month';
    IF hi >= now() - interval '30 days' OR lo = date_trunc('month', now()) THEN
        RETURN false;
    END IF;
    SELECT EXISTS (
        SELECT 1
        FROM (
            SELECT c.monitor_id, c.location_id, date_trunc('hour', c.checked_at) AS hour, count(*) AS n
            FROM check_results AS c
            WHERE c.checked_at >= lo AND c.checked_at < hi
            GROUP BY 1, 2, 3
        ) AS raw
        LEFT JOIN result_rollups_hourly AS r
          ON r.monitor_id = raw.monitor_id AND r.location_id = raw.location_id AND r.hour = raw.hour
        WHERE r.total IS DISTINCT FROM raw.n
    ) INTO incomplete;
    IF incomplete THEN
        RETURN false;
    END IF;
    EXECUTE format('ALTER TABLE check_results DETACH PARTITION %I', p_name);
    RETURN true;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION detach_expired_partition(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION detach_expired_partition(text) TO maiak_worker;

-- Drops one detached, expired partition. Refuses anything that is still attached, does not match
-- the naming pattern, is not a plain table owned by maiak_owner or is not expired, so it cannot
-- be used to drop another table. Returns false when there is nothing to drop.
-- +goose StatementBegin
CREATE FUNCTION drop_detached_partition(p_name text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
SET TimeZone = 'UTC'
SET lock_timeout = '5s'
AS $$
DECLARE
    lo timestamptz;
BEGIN
    IF p_name IS NULL OR p_name !~ '^check_results_p[0-9]{4}(0[1-9]|1[0-2])$' THEN
        RAISE EXCEPTION 'drop_detached_partition: invalid partition name' USING ERRCODE = '22023';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('maiak.ensure_partitions'));
    lo := make_timestamptz(substr(p_name, 16, 4)::integer, substr(p_name, 20, 2)::integer, 1, 0, 0, 0, 'UTC');
    IF lo + interval '1 month' >= now() - interval '30 days' OR lo = date_trunc('month', now()) THEN
        RETURN false;
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class AS c
        JOIN pg_namespace AS ns ON ns.oid = c.relnamespace AND ns.nspname = 'public'
        WHERE c.relname = p_name
          AND c.relkind = 'r'
          AND NOT c.relispartition
          AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
    ) THEN
        RETURN false;
    END IF;
    EXECUTE format('DROP TABLE %I', p_name);
    RETURN true;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION drop_detached_partition(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION drop_detached_partition(text) TO maiak_worker;
