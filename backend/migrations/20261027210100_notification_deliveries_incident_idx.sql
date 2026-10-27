-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';

-- The newest 'opened' or 'reminder' delivery of an incident to a channel, read by
-- enqueue_incident_reminders (S-01, ADR-0100). CONCURRENTLY: notification_deliveries keeps every
-- delivery ever queued.
CREATE INDEX CONCURRENTLY IF NOT EXISTS notification_deliveries_incident_channel_idx
    ON notification_deliveries (incident_id, channel_id, created_at)
    WHERE incident_id IS NOT NULL;
