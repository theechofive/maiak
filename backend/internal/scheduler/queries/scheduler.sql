-- name: GetActiveLocationIDByCode :one
SELECT id
FROM probe_locations
WHERE code = @code AND status = 'active';

-- name: InsertLocationState :exec
INSERT INTO monitor_location_state (tenant_id, monitor_id, location_id, next_run_at)
VALUES (@tenant_id, @monitor_id, @location_id, now() + make_interval(secs => @jitter_seconds::float8));

-- name: RestartMonitorLocations :execrows
-- Clearing the lease fences off a check that is still in flight from before the pause.
UPDATE monitor_location_state
SET status = 'unknown',
    consecutive_failures = 0,
    lease_token = NULL,
    lease_until = NULL,
    next_run_at = now() + make_interval(secs => @jitter_seconds::float8)
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;

-- name: ListLocationStates :many
SELECT location_id, status, consecutive_failures, last_checked_at
FROM monitor_location_state
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id
ORDER BY location_id;

-- name: UpdateLocationState :exec
UPDATE monitor_location_state
SET status = @status, consecutive_failures = @consecutive_failures, last_checked_at = @last_checked_at
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id AND location_id = @location_id;
