# Backend tasks for the MVP

Statuses are kept current in this file. Order follows the dependencies.

| ID | Task | Depends on | Status |
|---|---|---|---|
| B-01 | Platform: config, slog, DB pool, `WithTenantTx`/`WithTx`, UUIDv7, crypto (AES-GCM), clock, metrics, admin server (`/healthz`, `/readyz`, `/metrics`), graceful shutdown | none | doing |
| B-02 | Migration `init_tenancy_auth` (tenants, users, memberships, auth_identities, sessions, audit_log, RLS, grants, `auth_lookup_session`), `cmd/migrate` (goose, advisory lock), schema-lint test | B-01 | doing |
| B-03 | `maiakctl db bootstrap` (idempotent DB and roles, Secret `maiak-db` through the Kubernetes API) and `maiakctl hashpw` | B-01 | doing |
| B-04 | Auth: admin bootstrap from env, login/logout/me, sessions, CSRF, rate limit, audit rows | B-02 | doing |
| B-05 | OpenAPI contract for the whole MVP (`api/openapi.yaml`, the table in `README.md` of this folder), generated server code, problem+json mapping | B-01 | todo |
| B-06 | Dockerfile (multi-arch, five binaries), Makefile, golangci config with depguard, `check-generated` | B-01 | doing |
| B-07 | Migration `monitors_locations`: monitors, probe_locations (seed the home location), monitor_locations, monitor_location_state, monitor_state; RLS and grants; tenant limits (default 50 monitors, minimum interval 60 s) | B-02 | todo |
| B-08 | Monitors API: create, list, get, patch, delete (soft), pause, resume; validation (https URL, literal private IPs rejected, limits), audit rows | B-05, B-07 | todo |
| B-09 | SSRF guard and HTTPS prober (`checks`): connect-time IP validation, blocked ranges, at most 3 revalidated redirects, no proxy, no body read, TLS expiry capture, typed error kinds; table-driven tests including DNS rebinding (injectable resolver and dialer) | B-01 | todo |
| B-10 | Migration `results`: check_results partitioned by month, result_rollups_hourly, `ensure_partitions()`, `claim_due_monitors(n)` and `record_result(...)` as `SECURITY DEFINER` functions | B-07 | todo |
| B-11 | Incident state machine (pure package, test-first): per-location counters, quorum, thresholds, transitions, TLS-expiry warning decision; migration `incidents_outbox` (incidents, incident_monitors, notification_channels, monitor_channels, notification_deliveries) | B-10 | todo |
| B-12 | Worker: scheduler loop (claim batches, lease), bounded pool with per-tenant and per-host limits, batched result writes via the incident path in one transaction, graceful shutdown, metrics | B-09, B-10, B-11 | todo |
| B-13 | Notify: Slack webhook settings API (write-only, encrypted), test message, Slack notifier (opened, resolved, TLS expiring), outbox delivery with retries/backoff and per-destination rate limit in `jobs` | B-11 | todo |
| B-14 | Jobs: partition creation, hourly rollups, 30-day retention, outbox delivery loop; singleton tasks under advisory lock | B-10, B-13 | todo |
| B-15 | Read API for the dashboard (`internal/queries`): list with summary, stats (uptime, average, p95, series from rollups and recent raw rows), checks, incidents | B-08, B-10, B-11 | todo |
| B-16 | End-to-end integration test against Postgres: create monitor, run a worker iteration against a local TLS test server (test-only guard override), incident opens after threshold, outbox row created, resolved on recovery | B-12, B-13, B-15 | todo |
| B-17 | Metrics and logging pass: counters (checks, failures, scheduler lag, deliveries), request logs with `request_id`/`tenant_id`; `/metrics` on the admin port | B-12, B-13 | todo |
| B-18 | Docs and ledger sync: data model, runbooks, new entries for every shortcut | all | todo |

## Acceptance notes
- **B-09:** blocked ranges and redirect rules are exactly those of ADR-0011. The test-only override must not be reachable from production code paths (build tag or unexported option in tests).
- **B-10/B-11:** every new tenant table passes the schema-lint test (RLS enabled and forced, policy present); runtime roles never own objects.
- **B-12:** with two workers running, a monitor-location is never checked twice in the same interval (lease), and a crashed worker's lease expires so another picks it up.
- **B-13:** a delivery is retried with backoff up to 5 times, then marked failed with the last error; Slack webhook URLs never appear in logs or API responses.
- **B-15:** list endpoint answers from `monitor_state` and rollups without scanning raw results.
- **MVP limits:** interval fixed at 60 s and timeout at 10 s (enforced server-side), 50 monitors per tenant.
