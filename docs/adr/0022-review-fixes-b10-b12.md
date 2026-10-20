# ADR-0022: Decisions from the review of B-10..B-12

- Status: Accepted
- Date: 2026-10-20

## Context
The review found no blocker. Gaps: partitions exist only for three months and only `jobs` would create more; the worker flushes tenants serially; claims ignore tenant status and location; the outbox finish is not fenced; shutdown can exceed the pod grace period.

## Decision
- The worker also calls `ensure_partitions()` at startup and once per hour (idempotent, serialised by an advisory lock), so results never depend on `jobs` being deployed. `jobs` keeps its daily run.
- Result flushes run per tenant concurrently (bound 8) and the result transaction sets `lock_timeout = '2s'`; a lock timeout discards that tenant's batch like a stale lease.
- `claim_due_monitors(n, location)` takes the location code, joins `tenants` and skips non-active or deleted tenants.
- `claim_deliveries` returns a claim token; `finish_delivery` requires it; a requeue after lease expiry increments `attempts` so a poison message ends as failed.
- Partition retention detaches with `DETACH PARTITION ... CONCURRENTLY` (own transaction) and drops afterwards, to avoid an ACCESS EXCLUSIVE lock on the parent.
- `compute_rollups` takes a transaction-level advisory lock per call.
- The worker recovers from prober panics (slot released, no URL logged), uses the base context to release deferred leases, bounds the final flush to 15 s, and the chart sets `terminationGracePeriodSeconds: 45`.
- A permanent `ApplyResult` error completes the lease, increments an error metric and logs without secrets.
- A partial unique index allows at most one open incident per monitor.
- Flapping: the failure threshold is the only damping in the MVP; an `up` result resolves at once. Accepted and recorded in the ledger; a recovery threshold can be added later.

## Consequences
New migration(s) change two definer function signatures; the schema-lint allow-list and tests are updated. Every DB claim must be run once against Postgres in CI before it counts as verified.
