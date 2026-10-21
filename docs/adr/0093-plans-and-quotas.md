# ADR-0093: Plans and quotas: data model, resolution and enforcement

- Status: Accepted
- Date: 2026-10-21

## Context
ADR-0092 decided that every tenant has a plan with quotas (task P-01). Until now the only quota was a hard-coded default of 50 monitors with an optional `max_monitors` override in `tenants.limits` (ADR-0016). Registration (P-02) will create tenants on a free plan, heartbeat monitors (P-03) need their own quota, and the platform admin (P-04) will change plans.

## Decision
- **Data.** A global table `plans` (`key` primary key, `name`, `limits jsonb`, `created_at`), not tenant-scoped and outside RLS like `probe_locations`; the runtime roles may only read it, migrations and the owner change it. `tenants.plan_key` references it (`NOT NULL DEFAULT 'free'`). Seeded plans: `free` (`max_monitors` 3, `max_heartbeats` 1, `max_channels` 1) and `internal` (1000, 100, 10). The migration moves the existing `default` tenant to `internal`; `auth.BootstrapAdmin` creates `default` on `internal` and never changes the plan of an existing tenant.
- **Resolution.** `internal/plans` computes the effective limits: the plan's limits with every key present in `tenants.limits` taking precedence (the existing `max_monitors` override keeps working). Unknown keys are ignored. A known key must hold a non-negative JSON integer (`-1`, `1.5`, `4.0`, `"3"`, `null` are rejected); a key missing from the plan with no override, or a tenant on a plan that does not exist, is an error (HTTP 500, never a silent default). The hard-coded 50 is gone.
- **Enforcement.** `plans.CheckCreate(ctx, tx, tenantID, resource)` locks the tenant row (`SELECT ... FOR UPDATE`, as before), resolves the limits, counts the resource and returns `*plans.LimitError` when `count >= limit`. It runs in the transaction that inserts the row, so concurrent creates of one tenant are serialised and cannot overshoot. Resources: `monitors` (live monitors; soft-deleted ones free their slot, paused ones count), `channels` (rows of `notification_channels`; checked only when the Slack PUT would create the channel, replacing the webhook is always allowed), `heartbeats` (counted as 0 until P-03 adds the monitor type, L-181). New resource kinds add a constant and a count.
- **API.** `GET /api/v1/usage` returns the plan (`key`, `name`), the effective limits and the usage of the three resources. A quota failure is `409` with the problem type `urn:maiak:problem:plan-limit-reached` and the extension members `limit` (`monitors`, `heartbeats`, `channels`) and `max`. It replaces `urn:maiak:problem:limit-reached` of the 50-monitor limit; the problem type is not an enum in the contract, and the only client is our web app, changed in the same task.
- **Intervals stay fixed** (60 s interval, 10 s timeout) for every plan; plans carry no interval or retention limits yet (L-180).

## Consequences
- Quotas live in data: a new plan or a different free tier is a migration (or, with P-04, an admin action on the tenant), not a code change.
- Reading `monitors` and `notification_channels` from `internal/plans` is a read-only count; writes stay with the owning modules (CLAUDE.md rule 1).
- The Slack PUT now takes the tenant row lock too, so it is serialised with monitor creates of the same tenant (both are rare).
- The `internal` plan allows 1000 monitors, beyond the range the dashboard list was reasoned about (R-050, R-140).

## Alternatives considered
- Limits as columns of `plans`: every new resource would need a migration on two tables; jsonb with validation in one function keeps overrides and plans in the same shape.
- A database trigger or `CHECK` enforcing the count: harder to test, gives no structured error, and still needs the row lock for concurrency.
- Counting the Slack channel only through the unique index: the index allows one Slack channel per tenant, but it cannot express a plan with zero channels or several channel types later.
