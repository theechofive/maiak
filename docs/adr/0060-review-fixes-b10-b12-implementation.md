# ADR-0060: How B-20 implements the review fixes of ADR-0022

- Status: Accepted
- Date: 2026-10-20

## Context
ADR-0022 fixed what to change after the review of B-10..B-12. Implementing it (task B-20, migration `20261020180000_review_fixes_b10_b12.sql`) forced a few choices it did not make, and one of its decisions (`DETACH PARTITION ... CONCURRENTLY`) cannot be implemented under the role model of ADR-0004.

## Decision
- **Retention without `CONCURRENTLY` (amends ADR-0022).** `DETACH ... CONCURRENTLY` cannot run inside a function or a transaction block, and `ALTER TABLE` needs ownership of `check_results`, which `maiak_worker` (the role `jobs` runs as) must never have: an owner can disable `FORCE ROW LEVEL SECURITY`. Retention is therefore three definer functions driven by `jobs`, each called in its own short transaction: `list_droppable_partitions()` (catalog only: partitions `check_results_pYYYYMM` whose month ended more than 30 days ago, never the current month, attached or left detached by an interrupted run), `detach_expired_partition(name)` (re-validates the name, ownership and age, checks rollup completeness, then a plain `DETACH` under `lock_timeout = '1s'`) and `drop_detached_partition(name)` (drops only a detached, expired, owner-owned plain table matching the pattern). The ACCESS EXCLUSIVE lock on the parent is held only for the catalog change of the detach, and a busy parent makes the call fail fast and retry instead of queueing inserts behind it; the `DROP` never touches the parent. Names are validated with `^check_results_p[0-9]{4}(0[1-9]|1[0-2])$` and reach DDL only through `format('%I')`. `drop_expired_partitions()` is dropped. The housekeeping task keeps its name and lock; `jobs.Task.RunOutsideTx` runs it on other pool connections while the advisory-lock transaction stays open.
- **One open incident per monitor.** `incidents` had no monitor column (the link is `incident_monitors`). An automatic incident now also stores `monitor_id` (nullable, composite foreign key to `monitors`, backfilled for incidents with exactly one monitor), and `incidents_one_open_per_monitor_idx` is unique on `monitor_id` where `resolved_at IS NULL`. Manual incidents leave it NULL.
- **Claim filter.** `claim_due_monitors(n, p_location)` joins `tenants` (active, not deleted) and `probe_locations` (code = argument, active). The worker passes `MAIAK_WORKER_LOCATION` (default `home-eu-central-1`, DNS-label syntax). The old one-argument signature is dropped.
- **Outbox fencing.** `notification_deliveries.claim_token` is set by every claim (CHECK: present exactly while `in_progress`); `finish_delivery(id, token, status, error, retry_at)` updates only the row in progress under that token. Requeueing an expired claim counts an attempt with `last_error = 'lease_expired'` and marks the row `failed` at 5 attempts (`jobs.MaxAttempts`, kept in sync by a test).
- **Rollups.** `compute_rollups` takes `pg_advisory_xact_lock(hashtext('maiak.compute_rollups'))` before its upsert, so concurrent calls serialise and the second one reads the first one's result.
- **Worker.** `ensure_partitions()` at start and hourly (a failure is logged and counted in `maiak_worker_ensure_partitions_errors_total` and retried after a minute, never fatal). Tenant batches are flushed concurrently, at most 8 at a time; the result transaction sets `lock_timeout = '2s'`, and a lock timeout rolls back the whole tenant batch like a stale lease (`maiak_worker_lock_timeouts_total`; the leases expire and the checks run again). A prober panic is recovered: the limiter slot is released, the lease is left to expire, `maiak_worker_prober_panics_total` counts it, and the log has the monitor id but not the URL or the panic value. Deferred leases are released with a context that shutdown does not cancel. The final flush has 15 s in total. A permanent `ApplyResult` error (`incidents.ErrMonitorNotFound`, `incidents.ErrInconsistentState`: a missing monitor, state or location row, an incident to resolve that is already resolved, a second open incident) rolls back only the state-machine part (nested savepoint), keeps the result row and completes the lease, counts `maiak_worker_apply_errors_total` and logs ids and the error kind only.

## Consequences
- Two definer signatures change and one function is replaced, so the previous worker and jobs fail those calls during a rollout (L-090).
- Retention can still briefly block inserts for up to 1 s per expired partition and month (L-091).
- A slow send that outlives the 2-minute delivery lease now costs an attempt.
- Drain 25 s plus final flush 15 s fit in the chart's `terminationGracePeriodSeconds: 45` (chart task, ADR-0022).

## Alternatives considered
- `DETACH CONCURRENTLY` from `jobs` as `maiak_worker`: impossible without ownership of `check_results`; granting it would let a runtime role switch off RLS.
- Running retention from the owner-run migration hook: it runs only at deploy time, not daily.
- `DROP TABLE` of an attached partition (the previous code): holds the parent's ACCESS EXCLUSIVE lock for the whole drop and waited up to 5 s in the lock queue.
- A unique index on `incident_monitors`: it has no resolved flag; it would need the same denormalisation.
