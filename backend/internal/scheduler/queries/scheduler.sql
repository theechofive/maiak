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
