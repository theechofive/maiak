-- name: GetPlan :one
SELECT key, name, limits
FROM plans
WHERE key = @key;

-- name: CountLiveMonitors :one
-- Read-only count for the quota (monitors are owned by internal/monitors). Soft-deleted
-- monitors free their slot; paused ones still count.
SELECT count(*)
FROM monitors
WHERE tenant_id = @tenant_id AND deleted_at IS NULL;

-- name: CountChannels :one
-- Read-only count for the quota (channels are owned by internal/notify).
SELECT count(*)
FROM notification_channels
WHERE tenant_id = @tenant_id;
