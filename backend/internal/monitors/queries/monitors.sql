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
SELECT m.id, m.name, m.type, m.config, m.interval_seconds, m.timeout_seconds, m.failure_threshold,
       m.notify_slack, m.paused, m.created_at, m.updated_at,
       s.status, s.status_changed_at, s.last_error_kind, s.last_checked_at, s.tls_expires_at,
       s.current_incident_id
FROM monitors AS m
JOIN monitor_state AS s ON s.tenant_id = m.tenant_id AND s.monitor_id = m.id
WHERE m.tenant_id = @tenant_id AND m.id = @id AND m.deleted_at IS NULL;

-- name: LockMonitor :one
SELECT id, name, type, config, interval_seconds, timeout_seconds, failure_threshold, notify_slack, paused
FROM monitors
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL
FOR UPDATE;

-- name: UpdateMonitor :exec
UPDATE monitors
SET name = @name, config = @config, interval_seconds = @interval_seconds, failure_threshold = @failure_threshold,
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
-- Pause, resume and URL change. A new URL may be a different certificate, so its TLS fields go
-- too (clear_tls); on pause and resume they stay, so an already warned certificate is not warned
-- about again. maiak_api has column grants for exactly these columns (ADR-0018).
UPDATE monitor_state
SET status = 'unknown', status_changed_at = now(), last_error_kind = NULL,
    tls_expires_at = CASE WHEN @clear_tls::boolean THEN NULL ELSE tls_expires_at END,
    tls_warned_not_after = CASE WHEN @clear_tls::boolean THEN NULL ELSE tls_warned_not_after END
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;

-- name: InsertHeartbeatToken :exec
INSERT INTO heartbeat_tokens (tenant_id, monitor_id, token_hash)
VALUES (@tenant_id, @monitor_id, @token_hash);

-- name: ReplaceHeartbeatToken :execrows
-- Regenerate: the old hash is gone, so the old URL answers like any unknown token at once.
UPDATE heartbeat_tokens
SET token_hash = @token_hash, created_at = now()
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;

-- name: LockHeartbeat :one
-- Every change of a heartbeat's state (pause, resume, delete, evaluation in jobs) takes this row
-- lock first, so they are serialised (ADR-0094).
SELECT waiting_since, last_ping_at, applied_ping_at, missed_deadline
FROM heartbeat_tokens
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id
FOR UPDATE;

-- name: RestartHeartbeatWait :exec
-- Resume: the deadline counts from now, and pings from before the pause are not applied later.
UPDATE heartbeat_tokens
SET waiting_since = now(), applied_ping_at = last_ping_at
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;

-- name: MarkHeartbeatApplied :exec
UPDATE heartbeat_tokens
SET applied_ping_at = sqlc.narg('applied_ping_at'), missed_deadline = sqlc.narg('missed_deadline')
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;
