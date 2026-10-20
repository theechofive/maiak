# ADR-0020: Worker scheduling, concurrency limits and result batching

- Status: Accepted
- Date: 2026-10-20

## Context
Task B-12 turns `cmd/worker` into the check executor of the home location. ADR-0016 fixes the lease (30 s, fenced by `lease_token`) and the per-tenant result transaction. The worker must also cap checks in flight in total, per tenant and per target host, without one saturated tenant or host holding up everybody else, and must not let a job wait in the process until its lease expires (another worker would then run it again and the late result would be discarded).

## Decision
- **Claim only what can start.** Every second the scheduler loop claims `min(claim batch, free slots)` jobs, where free slots is the total concurrency minus checks in flight.
- **Non-blocking limiter.** Each claimed job asks the limiter for a slot (`TryAcquire(tenant, host)`: total, per tenant default 5, per host default 2). Admitted jobs start at once on their own goroutine.
- **Defer by giving the lease back.** A job whose tenant or host is at its limit is not queued in the process. Its lease is released immediately in a tenant transaction (`UPDATE ... WHERE lease_token = $token`), and `next_run_at` moves to at least now + 2 s, so a later tick (on any worker) claims it again and the claim, which is oldest first, does not keep returning the same saturated tenant's jobs ahead of other tenants. This needs no extra definer function: the worker role may update its tenant's schedule rows.
- **Batched writes.** Finished checks are buffered and written every 500 ms or at 50 results, one transaction per tenant (`db.WithTenantTx`), one savepoint per result. Inside a savepoint: insert the result, check and release the lease (`UPDATE ... lease_token = $1 AND lease_until > now()`, also setting `next_run_at = checked_at + interval + jitter of ±1 s`), run the incident state machine. A stale lease rolls back that savepoint only and increments `maiak_worker_stale_results_total`. The result row is inserted before the lease check so the transaction locks the monitor row (foreign key) before its schedule row, the order pause, resume and delete use, which rules out a deadlock with resume.
- **Shutdown.** On SIGTERM the loop stops claiming, waits up to 25 s for checks in flight, flushes, and exits 0. Checks run on a context that SIGTERM does not cancel. A check still running after the drain timeout is cancelled and its result dropped (not recorded as a failure); its lease expires and another worker repeats it.
- **Metrics** (admin port): `maiak_worker_checks_total{result}`, `maiak_worker_check_duration_seconds`, `maiak_worker_scheduler_lag_seconds` (claim time minus `next_run_at`, for started jobs), `maiak_worker_stale_results_total`, `maiak_worker_claim_errors_total`, `maiak_worker_write_errors_total`, `maiak_worker_deferred_jobs_total`.
- Configuration: `MAIAK_WORKER_CLAIM_BATCH` (100), `MAIAK_WORKER_CONCURRENCY` (50), `MAIAK_WORKER_TENANT_CONCURRENCY` (5), `MAIAK_WORKER_HOST_CONCURRENCY` (2). The prober uses `safehttp.NewClient` with `MAIAK_BLOCKED_CIDRS` and at most 3 redirects.

## Consequences
- A saturated tenant costs one claim and one release per deferred job and tick (R-030). The scheduler-lag histogram does not see the 2 s defer delays; `maiak_worker_deferred_jobs_total` shows them.
- A write error or a stale lease drops only that result; the job runs again when its lease has expired.
- The limits are per process; with several worker replicas the effective per-tenant and per-host concurrency is the limit times the replica count.

## Alternatives considered
- Per-tenant in-process queues: a queued job can outlive its 30 s lease, producing duplicate checks and discarded results.
- Letting the lease expire for jobs that cannot start: the job waits 30 s and the delay grows with load.
- A `release_lease` definer function: unnecessary, the release is a tenant-scoped update the worker role may already do.
- Blocking acquire on the limiter: one slow host or tenant would hold up the claim loop.
