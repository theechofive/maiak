-- name: CountLiveMonitors :one
SELECT count(*)
FROM monitors
WHERE tenant_id = @tenant_id AND deleted_at IS NULL;

-- name: InsertMonitor :exec
INSERT INTO monitors (id, tenant_id, name, type, config, interval_seconds, timeout_seconds, failure_threshold, notify_slack)
VALUES (@id, @tenant_id, @name, @type, @config, @interval_seconds, @timeout_seconds, @failure_threshold, @notify_slack);

-- name: InsertMonitorLocation :exec
INSERT INTO monitor_locations (tenant_id, monitor_id, location_id)
VALUES (@tenant_id, @monitor_id, @location_id);

-- name: InsertMonitorState :exec
INSERT INTO monitor_state (tenant_id, monitor_id)
VALUES (@tenant_id, @monitor_id);

-- name: GetMonitor :one
SELECT m.id, m.name, m.config, m.interval_seconds, m.timeout_seconds, m.failure_threshold,
       m.notify_slack, m.paused, m.created_at, m.updated_at,
       s.status, s.status_changed_at, s.last_error_kind, s.last_checked_at, s.tls_expires_at,
       s.current_incident_id
FROM monitors AS m
JOIN monitor_state AS s ON s.tenant_id = m.tenant_id AND s.monitor_id = m.id
WHERE m.tenant_id = @tenant_id AND m.id = @id AND m.deleted_at IS NULL;

-- name: ListMonitors :many
SELECT m.id, m.name, m.config, m.interval_seconds, m.timeout_seconds, m.failure_threshold,
       m.notify_slack, m.paused, m.created_at, m.updated_at,
       s.status, s.status_changed_at, s.last_error_kind, s.last_checked_at, s.tls_expires_at,
       s.current_incident_id
FROM monitors AS m
JOIN monitor_state AS s ON s.tenant_id = m.tenant_id AND s.monitor_id = m.id
WHERE m.tenant_id = @tenant_id AND m.deleted_at IS NULL
ORDER BY m.created_at, m.id;

-- name: LockMonitor :one
SELECT id, name, config, interval_seconds, timeout_seconds, failure_threshold, notify_slack, paused
FROM monitors
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL
FOR UPDATE;

-- name: UpdateMonitor :exec
UPDATE monitors
SET name = @name, config = @config, failure_threshold = @failure_threshold,
    notify_slack = @notify_slack, updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id;

-- name: SetMonitorPaused :exec
UPDATE monitors
SET paused = @paused, updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id;

-- name: SoftDeleteMonitor :exec
UPDATE monitors
SET deleted_at = now(), updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id;

-- name: ResetMonitorState :exec
UPDATE monitor_state
SET status = 'unknown', status_changed_at = now(), last_error_kind = NULL
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;
