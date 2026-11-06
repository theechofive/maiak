-- name: ListChannels :many
SELECT id, name, type, events, mention, reminder_minutes, status, created_at, updated_at, project_id
FROM notification_channels
WHERE tenant_id = @tenant_id AND deleted_at IS NULL
ORDER BY created_at, id;

-- name: GetChannelInfo :one
SELECT id, name, type, events, mention, reminder_minutes, status, created_at, updated_at, project_id
FROM notification_channels
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: LockChannel :one
-- Serialises changes of one channel (patch, delete, test outcome). NO KEY UPDATE: the key never
-- changes, so inserts into monitor_channels and notification_deliveries (FOR KEY SHARE on the
-- referenced row) are not blocked by it.
SELECT id, config_encrypted, status
FROM notification_channels
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL
FOR NO KEY UPDATE;

-- name: InsertChannel :exec
INSERT INTO notification_channels (id, tenant_id, type, name, config_encrypted, events, mention, reminder_minutes, project_id)
VALUES (@id, @tenant_id, 'slack_webhook', @name, @config_encrypted, @events, @mention, @reminder_minutes, sqlc.narg('project_id'));

-- name: UpdateChannel :exec
UPDATE notification_channels
SET name = @name, events = @events, mention = @mention, reminder_minutes = @reminder_minutes, updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: ReplaceChannelConfig :exec
-- A new webhook makes a failed channel active again; the id stays, so queued deliveries go to
-- the new URL.
UPDATE notification_channels
SET config_encrypted = @config_encrypted, status = 'active', updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: SoftDeleteChannel :exec
-- The webhook is wiped. status 'failed' keeps the previous release from using the channel
-- during a rollout (ADR-0100).
UPDATE notification_channels
SET deleted_at = now(), config_encrypted = NULL, status = 'failed', updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: DetachChannel :execrows
DELETE FROM monitor_channels
WHERE tenant_id = @tenant_id AND channel_id = @channel_id;

-- name: FailDeletedChannelDeliveries :one
SELECT fail_deleted_channel_deliveries(@channel_id::uuid)::integer;

-- name: GetChannel :one
-- Delivery lookup in jobs: deleted channels are returned too, so their deliveries fail with a
-- clear reason.
SELECT type, config_encrypted, status, mention, (deleted_at IS NOT NULL)::boolean AS deleted
FROM notification_channels
WHERE tenant_id = @tenant_id AND id = @id;

-- name: MarkChannelFailed :execrows
UPDATE notification_channels
SET status = 'failed', updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND status = 'active' AND deleted_at IS NULL;

-- name: LockLiveChannels :many
-- Channels a monitor may select. FOR SHARE waits for a concurrent delete or move and then sees
-- it, so a monitor is never linked to a channel deleted or moved meanwhile.
SELECT id, project_id
FROM notification_channels
WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]) AND deleted_at IS NULL
FOR SHARE;

-- name: ListMonitorChannelIDs :many
SELECT channel_id
FROM monitor_channels
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id
ORDER BY created_at, channel_id;

-- name: DeleteMonitorChannels :exec
DELETE FROM monitor_channels
WHERE tenant_id = @tenant_id AND monitor_id = @monitor_id;

-- name: InsertMonitorChannel :exec
INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id)
VALUES (@tenant_id, @monitor_id, @channel_id);

-- name: SetChannelProject :exec
-- NULL makes the channel organization-wide (T-02).
UPDATE notification_channels
SET project_id = sqlc.narg('project_id'), updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: DetachChannelFromOtherProjects :execrows
-- A channel moved to a project stays selected only by monitors of that project (T-02).
DELETE FROM monitor_channels AS mc
USING monitors AS m
WHERE mc.tenant_id = @tenant_id AND mc.channel_id = @channel_id
  AND m.tenant_id = mc.tenant_id AND m.id = mc.monitor_id
  AND m.project_id IS DISTINCT FROM @project_id::uuid;

-- name: DetachForeignChannels :execrows
-- A monitor moved to a project keeps only the selected channels of that project and the
-- organization-wide ones (T-02).
DELETE FROM monitor_channels AS mc
USING notification_channels AS c
WHERE mc.tenant_id = @tenant_id AND mc.monitor_id = @monitor_id
  AND c.tenant_id = mc.tenant_id AND c.id = mc.channel_id
  AND c.project_id IS NOT NULL AND c.project_id <> @project_id::uuid;
