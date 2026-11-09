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
-- events they receive (ADR-0100, T-02). A monitor with a valid selection notifies its selected
-- channels that are organization-wide or of its project; a monitor without a valid selection (none,
-- or only links to other projects' channels, which the api no longer creates and the T-02 contract
-- migration deleted) notifies the active channels of its own project only (closes L-261, L-263). The same rule is in enqueue_incident_reminders.
SELECT c.id, c.events
FROM notification_channels AS c
JOIN monitors AS m ON m.tenant_id = c.tenant_id AND m.id = @monitor_id
WHERE c.tenant_id = @tenant_id AND c.deleted_at IS NULL AND c.status = 'active'
  AND (
      (EXISTS (SELECT 1 FROM monitor_channels AS mc
               WHERE mc.tenant_id = c.tenant_id AND mc.monitor_id = m.id AND mc.channel_id = c.id)
       AND (c.project_id IS NULL OR c.project_id = m.project_id))
      OR (NOT EXISTS (SELECT 1 FROM monitor_channels AS mc
                      JOIN notification_channels AS c2 ON c2.tenant_id = mc.tenant_id AND c2.id = mc.channel_id
                      WHERE mc.tenant_id = c.tenant_id AND mc.monitor_id = m.id
                        AND (c2.project_id IS NULL OR c2.project_id = m.project_id))
          AND c.project_id = m.project_id)
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

-- name: DeliveryStillRouted :one
-- A queued delivery is sent only while its channel may still serve the monitor: organization-wide,
-- or of the monitor's current project (T-13). A move of either after the row was queued fails it.
SELECT EXISTS (
    SELECT 1
    FROM monitors AS m
    JOIN notification_channels AS c ON c.tenant_id = m.tenant_id AND c.id = @channel_id
    WHERE m.tenant_id = @tenant_id AND m.id = @monitor_id
      AND (c.project_id IS NULL OR c.project_id IS NOT DISTINCT FROM m.project_id)
)::boolean;
