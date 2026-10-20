# ADR-0051: Jobs process: outbox delivery and singleton housekeeping

- Status: Accepted
- Date: 2026-10-20

## Context
Task B-14 turns `cmd/jobs` into the outbox deliverer and the housekeeping runner. ADR-0016 fixes the definer functions (`claim_deliveries`, `finish_delivery`, `ensure_partitions`, `compute_rollups`, `drop_expired_partitions`, `purge_expired_sessions`) and the Slack failure semantics; ADR-0050 the notifier. `jobs` runs as `maiak_worker`, without `BYPASSRLS`, and must decrypt the webhook URL of a claimed delivery.

## Decision
- **Reading the channel: tenant transaction, no new definer function.** `claim_deliveries` returns `tenant_id`; `jobs` reads the channel with `notify.Service.DeliveryTarget` inside `db.WithTenantTx(tenant_id)`, so RLS still scopes the read and `maiak_worker` keeps only its existing `SELECT` and column `UPDATE (status, updated_at)` grants on `notification_channels`. A `get_delivery_target(delivery_id)` definer function would have been a second cross-tenant read path for the ciphertext with no gain. Decryption happens in Go with the keyring (only api and jobs have keys).
- **Delivery loop** (`internal/jobs.Deliverer`): every second claim `min(batch 20, free slots)` (concurrency 10) through `claim_deliveries`, one goroutine per row; no transaction is held while sending. Per row: resolve the channel (missing, undecryptable or `failed` channel → `failed` without sending: `channel_missing`, `channel_undecryptable`, `channel_failed`), format (`bad_payload` → failed), wait for the channel's rate limit (1 message per second per channel, in process), send, then `finish_delivery` in a tenant transaction together with flagging the channel `failed` on 404/410. `last_error` holds only an error kind.
- **Retries:** at most 5 attempts (every `finish_delivery` counts one, including 429). After the n-th failed attempt the next try is at `now + max(backoff(n), Retry-After)` with backoff 1 s, 5 s, 30 s, 2 min (10 min is the table's ceiling, reached only if `MaxAttempts` grows). The fifth failure is final.
- **Ordering and single flight** come from `claim_deliveries` (oldest undelivered row per monitor only, `SKIP LOCKED`); the jobs tests verify both with two concurrent deliverers and an opened/resolved pair with a failed first attempt.
- **Housekeeping** (`internal/jobs.Housekeeper`): each task runs at start and then every interval (`ensure_partitions` and `drop_expired_partitions` daily, `compute_rollups` every 5 minutes for the closed hours of the last 3 hours, `purge_expired_sessions` hourly; a failed run retries after 1 minute). A run is one transaction that first takes `pg_try_advisory_xact_lock(<task key>)`; if another replica holds it the run is skipped (`result="skipped"`). Transaction-scoped locks need no dedicated connection outside `platform/db` and are released by commit, rollback or a dead connection. All tasks are idempotent, so a replica running the same task later is harmless; this is mutual exclusion per run, not leader election.
- **Metrics** (admin port): `maiak_jobs_deliveries_total{status=sent|retry|failed}`, `maiak_jobs_delivery_duration_seconds`, `maiak_jobs_outbox_pending` (refreshed every 15 s by the new definer function `outbox_pending()`: count of pending rows, owner `maiak_owner`, `search_path` pinned, `EXECUTE` only for `maiak_worker`, allow-listed in the schema lint), `maiak_jobs_claim_errors_total`, `maiak_jobs_housekeeping_runs_total{task,result}`, `maiak_jobs_housekeeping_last_success_timestamp_seconds{task}`.
- **Shutdown:** on SIGTERM the loop stops claiming and waits up to 25 s for sends in flight (they run on a context SIGTERM does not cancel); after that they are cancelled and not recorded, their 2-minute lease expires and the row is delivered again (at-least-once, ADR-0007). A housekeeping run in progress is cancelled and rolls back.

## Consequences
- The channel rate limit and the concurrency are per jobs process; replicas multiply them (L-082).
- A 429 consumes one of the five attempts (L-082).
- `jobs` needs `MAIAK_PUBLIC_URL`, the encryption keys and `MAIAK_BLOCKED_CIDRS` (`config.LoadJobs`); the chart part is D-03 (L-042).

## Alternatives considered
- `get_delivery_target` definer function: broader (cross-tenant ciphertext read) than the tenant transaction.
- Session advisory lock held for the process lifetime (leader election): needs a pinned connection outside `platform/db` and a re-election path; per-run transaction locks are enough for idempotent tasks.
- Waiting for a rate-limit slot by re-queueing the row: `finish_delivery` always counts an attempt, so a burst would burn retries.
