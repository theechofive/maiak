# ADR-0090: Fixes from the final security review (B-21)

- Status: Accepted
- Date: 2026-10-20

## Context
The final security review of the MVP found two medium issues and several minor ones. (M1) The `method` label of the HTTP metrics took `r.Method` as sent by the client, so arbitrary tokens could grow the series without bound. (M2) `jobs` recomputed only the last 3 closed hours on every `compute_rollups` run (ADR-0051). After a `jobs` outage longer than that, the missed hours were never rolled up: the dashboard lost them (L-071), and retention kept their partition forever without saying so, because `detach_expired_partition` keeps a partition with incomplete rollups (ADR-0060).

## Decision
- **Method label.** `metrics.MethodLabel` keeps GET, HEAD, POST, PUT, PATCH, DELETE and OPTIONS. Every other method becomes `other`.
- **Rollup watermark.** Migration `20261020190000_rollup_watermark_review_fixes_b21.sql` adds `rollup_state`, a single-row table (`singleton boolean PRIMARY KEY CHECK (singleton)`, `watermark`, `updated_at`) owned by `maiak_owner`. It is system state, not tenant data, so it has no `tenant_id`, and the tenant RLS rules and the schema lint, which cover tables with a `tenant_id`, do not apply to it. The runtime roles get no privileges on it: the default DML grants are revoked. RLS is enabled without a policy, so even a later stray grant shows them no row. Only the new definer function `compute_rollups_catch_up(p_max_hours, p_lookback_hours)` reads and writes it. The function is owned by `maiak_owner` with `search_path` and `TimeZone` pinned, has `EXECUTE` for `maiak_worker` only and is allow-listed in the schema lint. It:
  1. takes the existing `compute_rollups` advisory lock before it reads the watermark;
  2. starts at the watermark. On the first run it starts at the earliest raw hour of the last 7 days, or at the current hour when there is none. The start is never older than 7 days;
  3. covers at most `p_max_hours` closed hours (1..168), plus the last `p_lookback_hours` closed hours (0..24) for late results;
  4. calls `compute_rollups(from, to)` for the upsert, so idempotency and the open-hour cap stay as they are;
  5. moves the watermark to the end of the covered range in the same transaction.

  `jobs` calls it every 5 minutes with 48 hours per run and a 3-hour lookback. A long outage is caught up 48 hours per run. `compute_rollups(from, to)` is kept unchanged (expand/contract), so the previous `jobs` release still works during a rollout and an operator can backfill older hours by hand.
- **Partitions kept back.** The retention task counts every expired partition it cannot drop in `maiak_jobs_partitions_kept_total{reason}`, where `reason` is `rollups_incomplete` (the detach refused) or `error` (the detach or drop failed). It logs the partition name with the reason at warn level.
- **Minor fixes.**
  - `POST /settings/slack/test` is limited to 1 call per 5 s per tenant. It uses the in-memory `auth.Limiter` (per api replica, like the login limit) and returns 429 problem+json with `Retry-After`. Every call counts, whatever its outcome.
  - A permanent `invalid_url` delivery failure marks the channel failed, like 404/410.
  - Monitor names reject Unicode format characters (category Cf: bidi overrides and isolates, zero-width characters, the BOM, soft hyphen, tag characters). This also rejects emoji sequences joined with U+200D.
  - Slack messages show the monitor URL without its query string and fragment. They keep the scheme, host (with port) and path, and drop user info. An unparsable URL is not shown.
  - Every API response carries `X-Content-Type-Options: nosniff`, set by the same middleware as `Cache-Control: no-store`.
  - `reset_monitor_runtime` skips soft-deleted monitors (`deleted_at IS NULL`). `CREATE OR REPLACE` keeps its owner and grants.
  - The `avgMs` descriptions in the spec now say what is averaged: checks that received a response. The value is null when none did.

## Consequences
- Hours older than 7 days that were never rolled up are not caught up automatically. Their partition is kept and counted until someone runs `compute_rollups(from, to)` for them (L-130).
- The first run after the deploy may recompute up to 7 days of hours that are already rolled up. This is idempotent, costs one scan, and is spread over several runs.
- The Slack test limit is per api replica (R-100).
- Rejecting U+200D makes some composed emoji unusable in monitor names.

## Alternatives considered
- A larger fixed lookback (for example 7 days on every run): every run would rescan days of raw rows to find a few missed hours.
- Finding unrolled hours by an anti-join of raw rows against rollups on every run: the same full scan as the retention completeness check.
- Keeping the watermark in a `jobs` process variable: lost on restart, which is exactly when catching up matters.
- A new `compute_rollups` overload instead of a new name: the allow-list and the call sites would not tell the two apart.
