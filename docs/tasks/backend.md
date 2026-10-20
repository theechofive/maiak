# Backend tasks for the MVP

Statuses are kept current in this file. Semantics for lifecycle, leases, rollups and Slack are in [ADR-0016](../adr/0016-mvp-semantics.md).

| ID | Task | Depends on | Status |
|---|---|---|---|
| B-01 | Platform: config (including `MAIAK_PUBLIC_URL`, `MAIAK_BLOCKED_CIDRS`), slog, DB pool, `WithTenantTx`/`WithTx`, UUIDv7, crypto (AES-GCM), clock, metrics, admin server (`/healthz`, `/readyz`, `/metrics`), graceful shutdown | none | done |
| B-02 | Migration `init_tenancy_auth` (tenants, users, memberships, auth_identities, sessions, audit_log, RLS, grants, `auth_lookup_session`), `cmd/migrate` (goose, advisory lock), schema-lint test (covers partition children and a definer-function allow-list) | B-01 | done |
| B-03 | `maiakctl db bootstrap` (idempotent DB and roles, Secret `maiak-db` through the Kubernetes API) and `maiakctl hashpw` | B-01 | done |
| B-04 | Auth: admin bootstrap from env, login/logout/me, sessions, CSRF, rate limit, audit rows | B-02 | done |
| B-05 | Full MVP OpenAPI contract (`api/openapi.yaml`, table in `README.md` of this folder plus the ADR-0016 additions), generated server code, `cmd/api` router wiring, problem+json mapping, a test that every unsafe operation except login requires the CSRF header | B-01, B-04 | todo |
| B-06 | Dockerfile (multi-arch, five binaries), Makefile (`generate`, `lint`, `test`, `build`, `check-generated`), golangci config with depguard | B-01 | done |
| B-07 | Migration `monitors_locations`: monitors (`paused`, `deleted_at`), probe_locations (seed the home location), monitor_locations, monitor_location_state (lease_token, lease_until, next_run_at), monitor_state (status, status_changed_at, last_error_kind, last_checked_at, tls_warned_not_after); RLS and grants; tenant limits (default 50 monitors, interval 60 s only) | B-02 | todo |
| B-08 | Monitors API per ADR-0016 lifecycle: create (one transaction, limit check with tenant row lock, initial state rows), list, get, patch, delete (soft), pause, resume; validation (https, port 443, literal private IPs rejected, limits), audit rows | B-05, B-07 | todo |
| B-09 | SSRF guard and HTTPS prober (`checks`): connect-time IP validation, blocked ranges plus `MAIAK_BLOCKED_CIDRS`, at most 3 revalidated redirects within the 10 s deadline, no proxy, no body read, port 443 only, TLS expiry capture, typed error kinds; table-driven tests including DNS rebinding (injectable resolver and dialer). The guarded HTTP client is a reusable package (used by B-13) | B-01 | todo |
| B-10 | Migration `results`: check_results partitioned by month (RLS on the parent and each child), result_rollups_hourly with latency histogram, definer functions `ensure_partitions()` (called by the migration for the current, next and following month), `claim_due_monitors(n)`, `compute_rollups`, `drop_expired_partitions`, `purge_expired_sessions`; tests including the month boundary | B-07 | todo |
| B-11 | Incident state machine (pure package, test-first): per-location counters, quorum, thresholds, transitions, TLS warning decision (one per certificate); migration `incidents_outbox` (incidents, incident_monitors, notification_channels, monitor_channels, notification_deliveries with kinds opened/resolved/tls_expiring) and definer functions `claim_deliveries(n)`, `finish_delivery(...)` | B-10 | todo |
| B-12 | Worker: scheduler loop (claim batches, lease fencing), bounded pool with per-tenant and per-host limits, one transaction per tenant batch (lease check, insert result, state machine, outbox), graceful shutdown, metrics. Test: stale-lease write is discarded; two workers never check the same job in one interval; a crashed worker's lease expires | B-09, B-10, B-11 | todo |
| B-13 | Slack channel: settings API (write-only, encrypted, URL must match `https://hooks.slack.com/services/...`), test message, Slack notifier package with the message formats from ADR-0016, 404/410 permanent failure, `Retry-After` handling | B-05, B-09, B-11 | todo |
| B-14 | Jobs: outbox delivery loop (ordered per monitor, retries with backoff up to 5, per-destination rate limit), partition maintenance, hourly rollups for closed hours, retention, expired-session purge; singleton tasks under advisory lock | B-10, B-13 | todo |
| B-15 | Read API for the dashboard (`internal/queries`): list with summary and `statusSince`/`lastErrorKind`/`lastCheckedAt`, detail with uptime 24 h/7 d/30 d, stats (average, p95, series; raw rows up to 24 h, rollups beyond), checks, incidents | B-08, B-10, B-11 | todo |
| B-16 | End-to-end integration test against Postgres: create a monitor, run a worker iteration against a local TLS test server (test-only guard override, not reachable from production code), incident opens after the threshold, outbox row created, delivered to a fake Slack, resolved on recovery | B-12, B-13, B-14, B-15 | todo |
| B-17 | Metrics and logging pass: counters (checks, failures, scheduler lag, deliveries), request logs with `request_id`/`tenant_id`; `/metrics` on the admin port | B-12, B-14 | todo |
| B-18 | Docs and ledger sync: data model, runbooks, new entries for every shortcut | all | todo |

## Acceptance notes
- **B-09:** blocked ranges and redirect rules are those of ADR-0011 as amended by ADR-0016. The test-only override must not be reachable from production code paths (build tag or unexported option used only in tests).
- **B-10/B-11:** every new tenant table, including partition children, passes the schema-lint test; runtime roles never own objects; every definer function pins `search_path` and has `EXECUTE` only for the intended role.
- **B-12:** results older than their lease are discarded and counted in a metric.
- **B-13/B-14:** Slack webhook URLs never appear in logs or API responses; opened and resolved for one monitor are delivered in order.
- **B-15:** the list endpoint answers from `monitor_state` and rollups without scanning raw results.
- **MVP limits:** interval fixed at 60 s, timeout 10 s, port 443 only, 50 monitors per tenant, enforced server-side.
