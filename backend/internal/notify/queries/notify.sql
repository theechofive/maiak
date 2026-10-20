-- name: GetSlackChannel :one
SELECT id, config_encrypted, status
FROM notification_channels
WHERE tenant_id = @tenant_id AND type = 'slack_webhook';

-- name: InsertSlackChannel :execrows
-- Does nothing when the tenant already has a Slack channel (one per tenant, ADR-0019); the
-- caller then replaces the existing row's config. ON CONFLICT waits for a concurrent insert, so
-- two concurrent PUTs end with one row.
INSERT INTO notification_channels (id, tenant_id, type, config_encrypted)
VALUES (@id, @tenant_id, 'slack_webhook', @config_encrypted)
ON CONFLICT (tenant_id) WHERE type = 'slack_webhook' DO NOTHING;

-- name: LockSlackChannel :one
SELECT id
FROM notification_channels
WHERE tenant_id = @tenant_id AND type = 'slack_webhook'
FOR UPDATE;

-- name: ReplaceChannelConfig :exec
-- A new webhook makes a failed channel active again; the id stays, so queued deliveries go to
-- the new URL.
UPDATE notification_channels
SET config_encrypted = @config_encrypted, status = 'active', updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetChannel :one
SELECT type, config_encrypted, status
FROM notification_channels
WHERE tenant_id = @tenant_id AND id = @id;

-- name: MarkChannelFailed :execrows
UPDATE notification_channels
SET status = 'failed', updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND status = 'active';
