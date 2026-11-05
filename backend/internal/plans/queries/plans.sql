-- name: GetPlan :one
SELECT key, name, limits
FROM plans
WHERE key = @key;

-- name: CountLiveMonitors :one
-- Read-only count for the quota (monitors are owned by internal/monitors). Soft-deleted
-- monitors free their slot; paused ones still count. Heartbeats have their own quota.
SELECT count(*)
FROM monitors
WHERE tenant_id = @tenant_id AND deleted_at IS NULL AND type = 'http';

-- name: CountLiveHeartbeats :one
SELECT count(*)
FROM monitors
WHERE tenant_id = @tenant_id AND deleted_at IS NULL AND type = 'heartbeat';

-- name: CountChannels :one
-- Read-only count for the quota (channels are owned by internal/notify). Deleted channels free
-- their slot.
SELECT count(*)
FROM notification_channels
WHERE tenant_id = @tenant_id AND deleted_at IS NULL;

-- name: CountMembers :one
-- Read-only count for the member quota (memberships and invitations are owned by internal/auth):
-- members plus open invitations, which reserve a place until they are accepted, revoked or
-- expire (ADR-0102).
SELECT ((SELECT count(*) FROM memberships AS m WHERE m.tenant_id = @tenant_id)
      + (SELECT count(*) FROM invitations AS i
         WHERE i.tenant_id = @tenant_id AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()))::bigint AS n;
