-- name: GetActiveLocationIDByCode :one
SELECT id
FROM probe_locations
WHERE code = @code AND status = 'active';

-- name: InsertLocationState :exec
INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at)
VALUES (@tenant_id, @monitor_id, @location_id, now() + make_interval(secs => @jitter_seconds::float8));

-- name: InsertParkedLocationState :exec
-- A heartbeat's location state is never claimed; it only carries the incident machine's
-- per-location status (ADR-0094).
INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at)
VALUES (@tenant_id, @monitor_id, @location_id, 'infinity');

-- name: ResetMonitorRuntime :one
-- Pause, resume and URL change: maiak_api may not write the schedule directly (ADR-0018), the
-- SECURITY DEFINER function resets it within the caller's tenant.
SELECT reset_monitor_runtime(@monitor_id::uuid);

-- name: ListLocationStates :many
SELECT location_id, status, consecutive_failures, last_checked_at
FROM monitor_location_state
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id
ORDER BY location_id;

-- name: UpdateLocationState :exec
UPDATE monitor_location_state
SET status = @status, consecutive_failures = @consecutive_failures, last_checked_at = @last_checked_at
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id AND location_id = @location_id;

-- name: CompleteLease :execrows
-- The lease check of ADR-0016: zero rows means the lease expired or was taken over, and the
-- caller discards its result. On success the next run is scheduled and the lease released; the
-- row stays locked until the result transaction commits, so nobody can claim it in between.
UPDATE monitor_location_state
SET next_run_at = @next_run_at, lease_token = NULL, lease_until = NULL
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id AND location_id = @location_id
  AND lease_token = @lease_token AND lease_until > now();

-- name: ReleaseLease :execrows
-- Gives a claimed job back unchecked (the worker had no capacity for it). It becomes due again
-- after a short delay, so the claim (oldest first) does not keep returning the same saturated
-- tenant's jobs ahead of everybody else's.
UPDATE monitor_location_state
SET lease_token = NULL, lease_until = NULL,
    next_run_at = greatest(next_run_at, now() + make_interval(secs => @delay_seconds::float8))
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id AND location_id = @location_id
  AND lease_token = @lease_token;
