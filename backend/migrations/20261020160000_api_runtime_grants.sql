-- +goose Up
SET LOCAL lock_timeout = '5s';

-- ADR-0018, ADR-0040: maiak_api no longer has table-wide UPDATE on the scheduler and state
-- tables. Lease fields (and the rest of the schedule) are written by the worker; the api resets
-- a monitor's schedule on pause, resume and URL change only through reset_monitor_runtime below.
-- monitor_state keeps column grants for what the api writes itself: the status reset on pause,
-- resume and URL change, and current_incident_id when it resolves an incident.
--
-- Expand/contract note (ledger L-070): an api pod of the previous release that resumes a monitor
-- while this migration has run but before the rollout replaced it gets a permission error
-- (HTTP 500) on that request; nothing is written.
REVOKE UPDATE ON monitor_location_state FROM maiak_api;
REVOKE UPDATE ON monitor_state FROM maiak_api;
GRANT UPDATE (status, status_changed_at, last_error_kind, tls_expires_at, tls_warned_not_after, current_incident_id)
    ON monitor_state TO maiak_api;

-- Resets the schedule of one monitor of the caller's tenant: every location back to status
-- unknown with zero consecutive failures, the lease dropped (which fences off a check still in
-- flight: its lease check fails and the result is discarded) and the next run after a random
-- start jitter of up to 30 s (scheduler.MaxStartJitter). The tenant comes from app.tenant_id,
-- set by db.WithTenantTx, never from an argument: the function runs as maiak_owner, whose
-- owner_claim_* policies see every tenant, so the explicit tenant filter is what keeps it inside
-- the caller's tenant. Returns the number of locations reset (0 when the monitor is not the
-- caller's).
-- +goose StatementBegin
CREATE FUNCTION reset_monitor_runtime(p_monitor_id uuid)
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
    WHERE m.tenant_id = caller_tenant AND m.id = p_monitor_id
      AND s.tenant_id = m.tenant_id AND s.monitor_id = m.id;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION reset_monitor_runtime(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION reset_monitor_runtime(uuid) TO maiak_api;
