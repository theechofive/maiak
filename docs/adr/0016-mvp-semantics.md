# ADR-0016: MVP semantics decided after the task-list review

- Status: Accepted
- Date: 2026-10-20

## Context
An independent review of the MVP task lists found gaps in who may do what under forced RLS, how a monitor lives and dies, how rollups stay correct, and what is sent to Slack. These decisions close them. They amend ADR-0004, ADR-0005, ADR-0007 and ADR-0011 where stated.

## Decisions

### Database functions for cross-tenant work (amends ADR-0004)
Runtime roles have no `BYPASSRLS`. Everything that spans tenants goes through `SECURITY DEFINER` functions owned by `maiak_owner`, with `SET search_path = public, pg_temp`, `EXECUTE` granted only to `maiak_worker` (not to `maiak_api`), and no dynamic SQL from arguments:
- `claim_due_monitors(n)` returns due (monitor, location) rows with a fresh `lease_token` and `lease_until`.
- `claim_deliveries(n)` returns pending outbox rows (oldest first, at most one in flight per monitor) and marks them in progress; `finish_delivery(id, status, error, retry_at)` records the outcome.
- `compute_rollups(from_hour, to_hour)` recomputes closed hours idempotently (upsert).
- `ensure_partitions()` creates the current, next and following month partitions (and enables and forces RLS with the standard policy on each child); `drop_expired_partitions()` drops a partition only when its upper bound is older than 30 days and all its hours are rolled up.
- `purge_expired_sessions()`.
Writing results for a claimed job is NOT a definer function: the worker opens one transaction per tenant batch (`SET LOCAL app.tenant_id`), checks the lease (`UPDATE monitor_location_state ... WHERE lease_token = $1 AND lease_until > now()`; zero rows means a stale lease and the result is discarded), inserts the result, runs the incident state machine and writes the outbox row.

### Leases
Lease duration is 30 s, longer than the 10 s request deadline (the deadline covers redirects). The `lease_token` fences stale writers.

### Monitor lifecycle
- Create (one transaction, through the owning services): `monitors` row, `monitor_locations` (home location), `monitor_location_state` (`next_run_at = now() + jitter`), `monitor_state` (status `unknown`). The 50-monitor limit counts non-deleted rows and is enforced after locking the tenant row (`SELECT ... FOR UPDATE`).
- The column is `paused boolean` (not `enabled`). `claim_due_monitors` skips paused and deleted monitors.
- Pause: an open incident is resolved with reason `paused` and no notification; counters stay. Resume: counters reset, status `unknown` until the first check.
- Delete: soft delete; an open incident is resolved with reason `deleted`, no notification; history is kept until retention; the monitor no longer counts toward the limit.
- Uptime percentage is computed from checks that actually ran, so paused windows are excluded by construction.

### TLS expiry warning (amends ADR-0007)
Outbox kinds are `opened`, `resolved` and `tls_expiring`. `monitor_state.tls_warned_not_after` records the certificate for which a warning was already sent; one warning per certificate when 14 days or less remain, and none for a certificate that was already warned about.

### Slack delivery (amends ADR-0011)
- The webhook URL must match `https://hooks.slack.com/services/...`. It is validated when saved and again before each send. The delivery goes through the same guarded dialer as the checks (no proxy, no redirects, connect-time IP validation).
- Permanent failure on 404 or 410 (delivery marked failed, channel flagged); `429` honours `Retry-After`; other errors retry with backoff up to 5 times.
- Messages (Slack mrkdwn): opened `:red_circle: *{name}* is down`, then the URL, reason, start time in UTC and `<{publicUrl}/monitors/{id}|Open in Maiak>`; resolved `:large_green_circle: *{name}* is back up after {duration}`; TLS `:warning: TLS certificate for *{name}* expires in {n} days ({date})`. `publicUrl` comes from `MAIAK_PUBLIC_URL`.
- Opened and resolved for one monitor are delivered in order.

### Checks (amends ADR-0011)
The MVP allows only port 443. Extra blocked CIDRs come from `MAIAK_BLOCKED_CIDRS` (comma-separated). The encryption keys are given only to `api` and `jobs`, not to `worker`.

### Rollups and retention (amends ADR-0005)
Hourly rollups store counts and a fixed log-scale latency histogram, so p95 can be merged across hours. Ranges up to 24 h are computed from raw rows, longer ones from rollups. Rollups cover only closed UTC hours. Because partitions are monthly, raw data is kept for at least 30 days (up to about 61 days); this is accepted and documented.

### API additions
List and detail responses include `statusSince`, `lastErrorKind` and `lastCheckedAt` (stored as `monitor_state.status_changed_at`, `last_error_kind`, `last_checked_at`); the detail response includes `uptime` for 24 h, 7 d and 30 d.

## Consequences
- More definer functions to review; each has a test and appears in the schema-lint allow-list.
- Retention is "at least 30 days", not exactly 30.
- Closes the open part of L-001 for the MVP (port 443 only); the rest stays open.
