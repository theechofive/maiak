# ADR-0019: MVP notification routing: one Slack channel per tenant, monitor_channels unused

- Status: Accepted
- Date: 2026-10-20

## Context
The data model has `notification_channels` and a link table `monitor_channels` so that, later, each monitor can choose its channels. The MVP has exactly one channel type (Slack Incoming Webhook) and a per-monitor `notify_slack` switch (B-07, B-08). Task B-11 had to decide which rows the incident service consults when it writes the outbox.

## Decision
- A tenant has at most one Slack channel (`notification_channels_one_slack_idx`, partial unique index on `tenant_id` where `type = 'slack_webhook'`).
- When the state machine emits a kind (`opened`, `resolved`, `tls_expiring`) and the monitor has `notify_slack = true`, the incident service writes one `notification_deliveries` row for the tenant's Slack channel if that channel exists and is `active`. Without an active channel nothing is queued; nothing is replayed when a channel is added later.
- `monitor_channels` is created now but is not read or written by the MVP. `notify_slack` plays its role. When per-monitor channel selection arrives, the service switches to `monitor_channels` and a data migration fills it from `notify_slack`.
- The outbox payload contains only what a message needs (monitor id and name, URL reduced to scheme, host and path, incident id, reason, start and end time, duration, certificate expiry and days left). The webhook URL stays encrypted in the channel and is resolved by `jobs` at delivery time.
- The TLS certificate counts as warned (`monitor_state.tls_warned_not_after`) when the decision fires, even if no delivery is queued, so configuring Slack later does not produce a burst of warnings for old certificates.

## Consequences
- Turning `notify_slack` on or adding a channel affects only future transitions; an incident that is already open does not get a late `opened` message (its `resolved` message is sent if a channel exists by then).
- Removing the one-channel limit is an index drop plus the `monitor_channels` switch.

## Alternatives considered
- Writing `monitor_channels` rows from `notify_slack` now: two sources of truth for the same switch, and nothing reads them yet.
- Queuing deliveries without a channel (to send once one exists): stale alerts and a delivery row that cannot reference a channel.
