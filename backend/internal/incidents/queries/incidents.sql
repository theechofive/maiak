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
-- monitor_id carries the partial unique index that allows one open incident per monitor.
INSERT INTO incidents (id, tenant_id, monitor_id, started_at, reason)
VALUES (@id, @tenant_id, @monitor_id, @started_at, @reason);

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

-- name: ListDeliveryChannels :many
-- Read-only (channels are owned by internal/notify): the active channels of a monitor with the
-- events they receive. A monitor without a selection in monitor_channels notifies every active
-- channel of its tenant (ADR-0100).
SELECT c.id, c.events
FROM notification_channels AS c
WHERE c.tenant_id = @tenant_id AND c.deleted_at IS NULL AND c.status = 'active'
  AND (
      EXISTS (SELECT 1 FROM monitor_channels AS mc
              WHERE mc.tenant_id = c.tenant_id AND mc.monitor_id = @monitor_id AND mc.channel_id = c.id)
      OR NOT EXISTS (SELECT 1 FROM monitor_channels AS mc
                     WHERE mc.tenant_id = c.tenant_id AND mc.monitor_id = @monitor_id)
  )
ORDER BY c.created_at, c.id;

-- name: IncidentStillDown :one
-- A queued reminder is sent only while its incident is open and its monitor notifies and is
-- neither paused nor deleted (ADR-0100).
SELECT EXISTS (
    SELECT 1
    FROM incidents AS i
    JOIN monitors AS m ON m.tenant_id = i.tenant_id AND m.id = i.monitor_id
    WHERE i.tenant_id = @tenant_id AND i.id = @incident_id AND i.resolved_at IS NULL
      AND m.notify_slack AND NOT m.paused AND m.deleted_at IS NULL
)::boolean;

-- name: InsertDelivery :exec
INSERT INTO notification_deliveries (id, tenant_id, kind, monitor_id, incident_id, channel_id, payload)
VALUES (@id, @tenant_id, @kind, @monitor_id, @incident_id, @channel_id, @payload);
