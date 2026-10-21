# Observability: metrics, logs and suggested alerts

Every process (`api`, `worker`, `jobs`) serves Prometheus metrics at `GET /metrics` on the admin port (`MAIAK_ADMIN_ADDR`, default `:9090`), next to `/healthz` and `/readyz`. The admin port is never routed through the gateway. Scraping is not set up yet (no `ServiceMonitor`, L-003); the alert rules below are suggestions written as text, not deployed.

## Label policy
No metric is labelled per tenant, monitor, incident, user, channel, URL, host, path, email, IP or token: label cardinality would grow with customers and the values would leak identifiers and URLs into the metrics stack. HTTP metrics use the chi route template (`/api/v1/monitors/{monitorId}`), never the raw path. `TestNoHighCardinalityOrSecretLabels` (`internal/platform/metrics`) scans the label names of every registered metric and fails on a forbidden one; `TestMetricsAreDocumented` fails when a `maiak_*` metric is missing from this page.

## Metrics

### All processes
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `maiak_build_info` | gauge (1) | `service`, `version`, `commit` (constant) | Which binary runs |
| `go_*`, `process_*` | various | | Go runtime and process collectors |

### api
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `maiak_http_requests_total` | counter | `route`, `method`, `code` | Requests by route template and status; `route="unmatched"` for paths no route matched; `method` is one of `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, else `other` |
| `maiak_http_request_duration_seconds` | histogram | `route`, `method` | Request latency |
| `maiak_auth_logins_total` | counter | `result` (`succeeded`, `failed`, `rate_limited`) | Login attempts |
| `maiak_auth_registrations_total` | counter | `result` (`succeeded`, `invalid`, `email_taken`, `rate_limited`) | Self-service registrations (P-02); a burst of `succeeded` while registration is open is the signal for the kill switch (R-130) |

### worker
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `maiak_worker_checks_total` | counter | `result` (`ok`, `failed`) | Checks run and recorded for writing |
| `maiak_worker_check_duration_seconds` | histogram | | One check including redirects |
| `maiak_worker_scheduler_lag_seconds` | histogram | | Claim time minus `next_run_at` |
| `maiak_worker_stale_results_total` | counter | | Results discarded because their lease expired or was taken over |
| `maiak_worker_lock_timeouts_total` | counter | | Tenant result batches discarded after the 2 s `lock_timeout` |
| `maiak_worker_apply_errors_total` | counter | | Results kept without a state-machine update (permanent error) |
| `maiak_worker_cancelled_checks_total` | counter | | Checks abandoned at shutdown and given back |
| `maiak_worker_write_errors_total` | counter | | Results that could not be written |
| `maiak_worker_claim_errors_total` | counter | | Failed `claim_due_monitors` calls |
| `maiak_worker_deferred_jobs_total` | counter | | Claimed jobs given back because their tenant or host was at its limit |
| `maiak_worker_prober_panics_total` | counter | | Recovered prober panics |
| `maiak_worker_ensure_partitions_errors_total` | counter | | Failed `ensure_partitions()` calls of the worker |

### jobs
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `maiak_jobs_deliveries_total` | counter | `status` (`sent`, `retry`, `failed`) | Delivery outcomes recorded by `finish_delivery` |
| `maiak_jobs_delivery_duration_seconds` | histogram | | One Slack send |
| `maiak_jobs_outbox_pending` | gauge | | Outbox rows waiting (including retries), all tenants; refreshed every 15 s |
| `maiak_jobs_claim_errors_total` | counter | | Failed `claim_deliveries` calls |
| `maiak_jobs_housekeeping_runs_total` | counter | `task`, `result` (`ok`, `skipped`, `error`) | Housekeeping runs; `task` is `ensure_partitions`, `compute_rollups`, `drop_expired_partitions` (the retention task, implemented by `list_droppable_partitions` / `detach_expired_partition` / `drop_detached_partition`) or `purge_expired_sessions` |
| `maiak_jobs_housekeeping_last_success_timestamp_seconds` | gauge | `task` | Unix time of the last successful run of each task |
| `maiak_jobs_partitions_kept_total` | counter | `reason` (`rollups_incomplete`, `error`) | Expired `check_results` partitions the retention task kept back; each one is also logged at warn level with the partition name (ADR-0090) |

## Logs
All processes write one JSON object per line to stdout (`log/slog`, level from `MAIAK_LOG_LEVEL`). Every line has `time`, `level`, `msg`, `service` and `version`.

Request log (api, `msg="http request"`, one per request):

| Field | Content |
|---|---|
| `request_id` | The incoming `X-Request-Id` when it matches `[A-Za-z0-9._-]{1,128}`, otherwise a new UUIDv7; returned in the `X-Request-Id` response header and in problem bodies (`request_id`) |
| `tenant_id` | Set once the session is authenticated (and by a successful login); absent for anonymous requests |
| `method` | HTTP method |
| `route` | Route template, `unmatched` when none matched |
| `status`, `bytes` | Response status and body size |
| `duration_ms` | Handler duration in milliseconds |

Other lines of a request carry the same `request_id` and `tenant_id`. The worker and jobs log ids (`tenant_id`, `monitor_id`, `location_id`, `delivery_id`, `channel_id`) and error kinds.

Never logged, by design and by test (`TestIntegrationRequestLogsCarryNoSecrets` in `internal/api`, the log assertions of `internal/e2e` and of the worker and jobs tests): passwords, session tokens, CSRF tokens, monitor URLs (they can carry credentials in the query), Slack webhook URLs (rejected values included), raw paths and query strings, request and response bodies, and raw errors of the SSRF guard (they contain resolved addresses and the URL; only error kinds such as `blocked_address` or `timeout` are logged and stored).

## Suggested alert rules (not deployed)
Written for the VictoriaMetrics/Alertmanager stack of L-003; thresholds are first guesses.

| Alert | Expression (PromQL) | For | Why |
|---|---|---|---|
| ChecksStalled | `sum(rate(maiak_worker_checks_total[5m])) == 0` and at least one active monitor | 5m | Nothing is being checked (the self-monitoring rule of L-003) |
| SchedulerLagHigh | `histogram_quantile(0.95, sum(rate(maiak_worker_scheduler_lag_seconds_bucket[10m])) by (le)) > 30` | 10m | Checks start late: worker saturated or claims slow |
| ResultsDiscarded | `sum(rate(maiak_worker_stale_results_total[15m])) + sum(rate(maiak_worker_lock_timeouts_total[15m])) > 0.1` | 15m | Results are thrown away (lease expiry, lock contention) |
| WorkerErrors | `increase(maiak_worker_write_errors_total[15m]) + increase(maiak_worker_claim_errors_total[15m]) + increase(maiak_worker_apply_errors_total[15m]) + increase(maiak_worker_prober_panics_total[15m]) > 0` | 0m | Any write, claim, apply or prober failure needs a look |
| PartitionsFailing | `increase(maiak_worker_ensure_partitions_errors_total[2h]) > 1` or `time() - maiak_jobs_housekeeping_last_success_timestamp_seconds{task="ensure_partitions"} > 2*86400` | 0m | Without future partitions result inserts fail at the next month (L-040) |
| HousekeepingStale | `time() - maiak_jobs_housekeeping_last_success_timestamp_seconds{task="compute_rollups"} > 1800` (and for `drop_expired_partitions` and `purge_expired_sessions`: `> 2*86400`, `> 3*3600`) | 0m | A singleton task stopped succeeding |
| RetentionErrors | `increase(maiak_jobs_housekeeping_runs_total{task="drop_expired_partitions",result="error"}[1d]) > 2` | 0m | The detach keeps hitting its lock timeout (L-091) |
| PartitionsKept | `increase(maiak_jobs_partitions_kept_total{reason="rollups_incomplete"}[2d]) > 0` | 0m | An expired month still has hours without rollups: older than the 7-day catch-up, needs a manual `compute_rollups(from, to)` (L-130) |
| OutboxBacklog | `maiak_jobs_outbox_pending > 50` | 10m | Alerts are not leaving: Slack down, rate limits or jobs stuck |
| DeliveriesFailing | `sum(rate(maiak_jobs_deliveries_total{status="failed"}[1h])) > 0` | 0m | A delivery gave up after five attempts or its channel is unusable |
| APIErrors | `sum(rate(maiak_http_requests_total{code=~"5.."}[5m])) / sum(rate(maiak_http_requests_total[5m])) > 0.05` | 10m | api failing |
| APILatency | `histogram_quantile(0.95, sum(rate(maiak_http_request_duration_seconds_bucket{route!="unmatched"}[10m])) by (le, route)) > 1` | 15m | Slow dashboard reads (R-050) |
| LoginAbuse | `sum(rate(maiak_auth_logins_total{result="rate_limited"}[15m])) > 0.2` | 15m | Password guessing or a lockout (R-013) |
| RegistrationBurst | `sum(increase(maiak_auth_registrations_total{result="succeeded"}[1h])) > 20` | 0m | Mass sign-ups; consider `config.registrationEnabled: false` (R-130) |
