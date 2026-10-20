-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Raw check results, partitioned by month on checked_at (ADR-0005, ADR-0016). Owning module:
-- internal/results. No row id: the key is (monitor, time, location), which also serves the
-- "latest checks of a monitor" query (monitor_id, checked_at DESC). Retention is DROP of whole
-- monthly partitions (drop_expired_partitions below); the BRIN index serves the time-range scans
-- of compute_rollups and costs almost nothing to maintain on append-only data.
-- latency_ms is NULL when no response arrived (DNS, connect, TLS, timeout).
CREATE TABLE check_results (
    tenant_id      uuid        NOT NULL,
    monitor_id     uuid        NOT NULL,
    location_id    uuid        NOT NULL,
    checked_at     timestamptz NOT NULL,
    ok             boolean     NOT NULL,
    status_code    smallint    CHECK (status_code BETWEEN 100 AND 599),
    latency_ms     integer     CHECK (latency_ms >= 0),
    error_kind     text        CHECK (length(error_kind) BETWEEN 1 AND 64),
    tls_not_after  timestamptz,
    PRIMARY KEY (monitor_id, checked_at, location_id),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id),
    CHECK (ok = (error_kind IS NULL))
) PARTITION BY RANGE (checked_at);
CREATE INDEX check_results_checked_at_brin ON check_results USING brin (checked_at);
-- Append-only: the worker inserts, the api reads. Partitions repeat these grants.
REVOKE INSERT, UPDATE, DELETE ON check_results FROM maiak_api;
REVOKE UPDATE, DELETE ON check_results FROM maiak_worker;

-- Hourly rollups per (monitor, location, hour), kept 13 months. Owning module: internal/results.
-- latency_histogram has 11 buckets with fixed upper edges in milliseconds (inclusive):
--   10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, and an overflow bucket (> 10000).
-- The edges are mirrored by results.LatencyBucketEdgesMs in Go; a test keeps them in sync.
-- Only results with a latency are counted in the histogram and latency_sum_ms, so the mean is
-- latency_sum_ms / sum(latency_histogram) and p95 can be merged across hours by adding arrays.
CREATE TABLE result_rollups_hourly (
    tenant_id          uuid        NOT NULL,
    monitor_id         uuid        NOT NULL,
    location_id        uuid        NOT NULL,
    hour               timestamptz NOT NULL CHECK (hour = date_trunc('hour', hour, 'UTC')),
    total              integer     NOT NULL CHECK (total > 0),
    ok_count           integer     NOT NULL CHECK (ok_count BETWEEN 0 AND total),
    latency_sum_ms     bigint      NOT NULL CHECK (latency_sum_ms >= 0),
    latency_histogram  integer[]   NOT NULL CHECK (cardinality(latency_histogram) = 11),
    computed_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (monitor_id, location_id, hour),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
);
CREATE INDEX result_rollups_hourly_hour_idx ON result_rollups_hourly (hour);
-- Written only by compute_rollups (as maiak_owner); runtime roles read.
REVOKE INSERT, UPDATE, DELETE ON result_rollups_hourly FROM maiak_api, maiak_worker;

ALTER TABLE check_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE check_results FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON check_results
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE result_rollups_hourly ENABLE ROW LEVEL SECURITY;
ALTER TABLE result_rollups_hourly FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON result_rollups_hourly
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The cross-tenant SECURITY DEFINER functions below run as maiak_owner, which has no BYPASSRLS
-- and is subject to FORCE ROW LEVEL SECURITY like everybody else. These policies, limited to
-- maiak_owner and to the commands each function needs, let them see across tenants (same
-- pattern as owner_session_lookup, ADR-0004). Runtime roles are never members of maiak_owner
-- (schema-lint), so the policies do not widen their access. Reads of check_results always go
-- through the parent, whose policies apply to all partitions.
CREATE POLICY owner_claim_read ON monitors FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_claim_read ON monitor_location_state FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_claim_lease ON monitor_location_state FOR UPDATE TO maiak_owner USING (true) WITH CHECK (true);
CREATE POLICY owner_rollup_read ON check_results FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_rollup_write ON result_rollups_hourly TO maiak_owner USING (true) WITH CHECK (true);
CREATE POLICY owner_session_purge ON sessions FOR DELETE TO maiak_owner USING (expires_at < now());

-- Creates the check_results partitions for the current, next and following UTC month if they
-- are missing; a new partition gets RLS (enabled, forced, tenant policy) and the same grants as
-- the parent in the same transaction, so it is never visible without them. Idempotent and
-- serialised by a transaction advisory lock. Returns the number of partitions created.
-- Partition names are check_results_pYYYYMM; drop_expired_partitions relies on that.
-- +goose StatementBegin
CREATE FUNCTION ensure_partitions()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
SET TimeZone = 'UTC'
SET lock_timeout = '5s'
AS $$
DECLARE
    first_month timestamptz := date_trunc('month', now());
    lo          timestamptz;
    hi          timestamptz;
    part        text;
    created     integer := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('maiak.ensure_partitions'));
    FOR i IN 0..2 LOOP
        lo := first_month + make_interval(months => i);
        hi := first_month + make_interval(months => i + 1);
        part := 'check_results_p' || to_char(lo, 'YYYYMM');
        CONTINUE WHEN to_regclass('public.' || quote_ident(part)) IS NOT NULL;
        EXECUTE format('CREATE TABLE %I PARTITION OF check_results FOR VALUES FROM (%L) TO (%L)', part, lo, hi);
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', part);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', part);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, part);
        EXECUTE format('REVOKE ALL ON %I FROM maiak_api, maiak_worker', part);
        EXECUTE format('GRANT SELECT ON %I TO maiak_api', part);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO maiak_worker', part);
        created := created + 1;
    END LOOP;
    RETURN created;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION ensure_partitions() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ensure_partitions() TO maiak_worker;

SELECT ensure_partitions();

-- Claims up to n due (monitor, location) pairs across all tenants: not paused, not deleted,
-- next_run_at reached, lease free or expired. Each claimed row gets a fresh lease_token
-- generated here and a 30 s lease (ADR-0016); the token fences off a stale writer. SKIP LOCKED
-- lets several workers claim concurrently without waiting on each other or double-claiming.
-- next_run_at is returned unchanged so the caller can measure scheduler lag.
-- +goose StatementBegin
CREATE FUNCTION claim_due_monitors(n integer)
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
        WHERE s.next_run_at <= now()
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
REVOKE ALL ON FUNCTION claim_due_monitors(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_due_monitors(integer) TO maiak_worker;

-- Recomputes the rollups of the closed UTC hours in [from_hour, to_hour): to_hour is capped at
-- the start of the current hour, so an open hour is never rolled up. Upsert, so running it again
-- (or after late results) gives the same numbers as one run over all raw rows. Histogram
-- buckets: see result_rollups_hourly. Returns the number of rollup rows written.
-- +goose StatementBegin
CREATE FUNCTION compute_rollups(from_hour timestamptz, to_hour timestamptz)
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
REVOKE ALL ON FUNCTION compute_rollups(timestamptz, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION compute_rollups(timestamptz, timestamptz) TO maiak_worker;

-- Drops check_results partitions whose upper bound is more than 30 days in the past, but only
-- when every (monitor, location, hour) in the partition has a rollup with the same total, i.e.
-- the rollups are complete (or the partition is empty). A partition with unrolled or late rows is
-- kept until compute_rollups has covered it. Only partitions named check_results_pYYYYMM (made by
-- ensure_partitions) are considered. The completeness check scans the partition once; fine at
-- the MVP volume, see L-013 for the scale path. Returns the number of partitions dropped.
-- +goose StatementBegin
CREATE FUNCTION drop_expired_partitions()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
SET TimeZone = 'UTC'
SET lock_timeout = '5s'
AS $$
DECLARE
    part       text;
    lo         timestamptz;
    hi         timestamptz;
    incomplete boolean;
    dropped    integer := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('maiak.ensure_partitions'));
    FOR part IN
        SELECT c.relname
        FROM pg_inherits AS i
        JOIN pg_class AS c ON c.oid = i.inhrelid
        WHERE i.inhparent = 'public.check_results'::regclass
          AND c.relname ~ '^check_results_p[0-9]{6}$'
        ORDER BY c.relname
    LOOP
        lo := make_timestamptz(substr(part, 16, 4)::integer, substr(part, 20, 2)::integer, 1, 0, 0, 0, 'UTC');
        hi := lo + interval '1 month';
        CONTINUE WHEN hi >= now() - interval '30 days';
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
        CONTINUE WHEN incomplete;
        EXECUTE format('DROP TABLE %I', part);
        dropped := dropped + 1;
    END LOOP;
    RETURN dropped;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION drop_expired_partitions() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION drop_expired_partitions() TO maiak_worker;

-- Deletes expired sessions (L-025); run by jobs. Returns the number deleted.
-- +goose StatementBegin
CREATE FUNCTION purge_expired_sessions()
RETURNS integer
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    WITH d AS (DELETE FROM sessions WHERE expires_at < now() RETURNING 1)
    SELECT count(*)::integer FROM d
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION purge_expired_sessions() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION purge_expired_sessions() TO maiak_worker;
