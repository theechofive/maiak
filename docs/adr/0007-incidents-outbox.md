# ADR-0007: Incident state machine and transactional outbox

- Status: Accepted
- Date: 2026-10-20

## Context
Alerts must never be lost and state must never disagree with what was sent. Flapping must not spam channels.

## Decision
A check is considered down after N consecutive failures in a location (default 2, configurable per monitor). The aggregated monitor status follows a quorum rule. A transition up to down or back opens or resolves an `incident` and inserts a `notification_deliveries` row in the same transaction as the state update. `jobs` delivers from the outbox (`SKIP LOCKED`) with retries and backoff and a per-destination rate limit. Slack messages for opened and resolved incidents and one TLS-expiry warning per certificate (amended by ADR-0016), no reminders. Slack is configured by Incoming Webhook URL, stored encrypted. The state machine runs synchronously in the result-write path and is developed test-first.

## Consequences
- Alert delivery is at-least-once; duplicates must be tolerated by the formatter or deduplicated by delivery id.
- Alert delivery capacity is independent of probe load because `jobs` is a separate process.
- A Slack OAuth app instead of webhooks is deferred (L-014).

## Alternatives considered
- Asynchronous evaluation by a separate consumer: more moving parts, state and alerts can diverge.
- Sending to Slack inside the check transaction: couples availability of Slack to checks.
