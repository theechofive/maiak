-- name: GetMonitorForResult :one
SELECT name, config, failure_threshold, notify_slack, paused, (deleted_at IS NOT NULL)::boolean AS deleted
FROM monitors
WHERE tenant_id = @tenant_id AND id = @id;

-- name: LockMonitorState :one
-- Serialises results of different locations of one monitor and pause/delete.
SELECT status, status_changed_at, last_error_kind, last_checked_at, tls_expires_at, tls_warned_not_after,
       current_incident_id
FROM monitor_state
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id
FOR UPDATE;

-- name: UpdateMonitorState :exec
UPDATE monitor_state
SET status = @status,
    status_changed_at = @status_changed_at,
    last_error_kind = @last_error_kind,
    last_checked_at = @last_checked_at,
    tls_expires_at = @tls_expires_at,
    tls_warned_not_after = @tls_warned_not_after,
    current_incident_id = @current_incident_id
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;

-- name: ClearCurrentIncident :exec
UPDATE monitor_state
SET current_incident_id = NULL
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;

-- name: InsertIncident :exec
INSERT INTO incidents (id, tenant_id, started_at, reason)
VALUES (@id, @tenant_id, @started_at, @reason);

-- name: InsertIncidentMonitor :exec
INSERT INTO incident_monitors (tenant_id, incident_id, monitor_id)
VALUES (@tenant_id, @incident_id, @monitor_id);

-- name: ResolveIncident :one
-- resolved_at defaults to the database clock (pause, delete) and never precedes started_at,
-- which comes from the worker's clock.
UPDATE incidents
SET resolved_at = greatest(coalesce(sqlc.narg('resolved_at')::timestamptz, now()), started_at),
    resolve_reason = @resolve_reason
WHERE tenant_id = @tenant_id AND id = @id AND resolved_at IS NULL
RETURNING started_at, resolved_at;

-- name: GetActiveSlackChannel :one
SELECT id
FROM notification_channels
WHERE tenant_id = @tenant_id AND type = 'slack_webhook' AND status = 'active';

-- name: InsertDelivery :exec
INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, incident_id, channel_id, payload)
VALUES (@id, @tenant_id, @kind, @monitor_id, @incident_id, @channel_id, @payload);
