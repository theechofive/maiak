# ADR-0021: How the results migration implements the definer functions

- Status: Accepted
- Date: 2026-10-20

## Context
ADR-0005 and ADR-0016 fix what the results storage does (monthly partitions, hourly rollups with a histogram, cross-tenant work only through `SECURITY DEFINER` functions owned by `maiak_owner`). Implementing task B-10 forced a few choices that the earlier ADRs did not make.

## Decision
- **Owner-scoped policies.** `maiak_owner` has no `BYPASSRLS`, and `FORCE ROW LEVEL SECURITY` applies to it, so a definer function would see no rows without a tenant set. Each table a definer function touches gets a permissive policy limited to `maiak_owner` and to the command the function needs (`owner_claim_read` / `owner_claim_lease` on the schedule, `owner_claim_read` on `monitors`, `owner_rollup_read` on `check_results`, `owner_rollup_write` on `result_rollups_hourly`, `owner_session_purge` on `sessions` for expired rows only). It is the same pattern as `owner_session_lookup` (ADR-0004). The schema-lint test allow-lists each one by name, command and role; runtime roles are never members of `maiak_owner`, so these policies do not widen their access.
- **Partitions.** Children are named `check_results_pYYYYMM` and created by `ensure_partitions()` together with RLS (enabled, forced, tenant policy) and grants in one transaction. Reads in definer functions always go through the parent, so children need only the tenant policy. No default partition: a result outside the created months fails loudly instead of landing in a partition that later blocks `ATTACH`.
- **UTC by construction.** The functions that compute month and hour boundaries pin `TimeZone = 'UTC'` (and `lock_timeout = '5s'` where they take DDL locks), so month arithmetic never depends on the session time zone.
- **Histogram.** 11 buckets with inclusive upper edges 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000 ms and an overflow bucket; mirrored in Go as `results.LatencyBucketEdgesMs`. Only results with a latency (a response arrived) are counted in the histogram and `latency_sum_ms`; `total` and `ok_count` count every result. The mean is `latency_sum_ms / sum(histogram)`.
- **Drop only complete partitions.** `drop_expired_partitions()` drops a partition whose upper bound is more than 30 days old only when, for every (monitor, location, hour) with raw rows, a rollup with the same `total` exists. Late rows after a rollup keep the partition until `compute_rollups` runs again.
- **Claim output.** `claim_due_monitors(n)` also returns `interval_seconds`, `timeout_seconds` and the unchanged `next_run_at`, so the worker needs no second query for the check parameters and can measure scheduler lag. The batch is capped at 1000.

## Consequences
- A bug in a definer function can read or write across tenants; every function has an integration test and must stay free of dynamic SQL built from arguments (partition names come from dates and `pg_class`).
- The completeness check scans an expired partition once a month (L-040).
- If `jobs` does not run `ensure_partitions()` for two months, inserts fail (L-040).

## Alternatives considered
- A `BYPASSRLS` maintenance role: not grantable without a real superuser on RDS, and broader than per-command policies.
- Setting `app.tenant_id` per tenant inside the functions: claiming across tenants with one `SKIP LOCKED` query is the point.
- A default partition as a safety net: rows in it block creating the matching monthly partition later and are never dropped by retention.
- Counting failed checks without a response in the overflow bucket: it would distort p95 of real responses.
