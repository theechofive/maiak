# ADR-0040: How B-19 implements the review fixes of ADR-0018

- Status: Accepted
- Date: 2026-10-20

## Context
ADR-0018 fixed what to change after the review of B-05..B-09. Implementing it (task B-19) forced a few choices it did not make, and one of them amends ADR-0016.

## Decision
- **One reset for pause, resume and URL change.** `reset_monitor_runtime(monitor_id uuid)` (`SECURITY DEFINER`, owner `maiak_owner`, `search_path` pinned, `EXECUTE` only for `maiak_api`) resets every location of the monitor: status `unknown`, zero consecutive failures, lease dropped, next run after a random jitter of up to 30 s computed in SQL. It reads the tenant from `app.tenant_id` (it raises when unset) and filters on it explicitly, because the owner's `owner_claim_*` policies would otherwise let it see every tenant; for another tenant's monitor it changes nothing and returns 0. The scheduler module calls it (`scheduler.Service.Restart`), so `monitor_location_state` keeps one owner.
- **Pause resets too (amends ADR-0016 "counters stay").** Pause calls the same reset, which drops the lease and so fences a check in flight; its status becomes `unknown` with `statusSince` at the pause. Keeping the counters had no visible effect: claims skip paused monitors and resume resets them anyway.
- **Grants.** `maiak_api` loses `UPDATE` on `monitor_location_state` entirely and keeps column grants on `monitor_state` for `status`, `status_changed_at`, `last_error_kind`, `tls_expires_at`, `tls_warned_not_after` and `current_incident_id` (the reset and `incidents.ResolveOpen`). The worker's grants are unchanged.
- **URL change.** A PATCH that changes `url` does the reset, clears both TLS fields (a new target may serve a new certificate) and resolves an open incident with `url_changed`, without a notification. A PATCH of other fields leaves the runtime state alone. Pause and resume keep the TLS fields so a certificate already warned about is not warned about again.
- **Cancelled checks.** `checks.Result.Cancelled` is set when the caller's context was cancelled (not on the check's own deadline); `OK` is false and `ErrorKind` empty, so such a result cannot even be inserted by mistake (`ok = (error_kind IS NULL)`). The worker discards it and releases the lease at once (`maiak_worker_cancelled_checks_total`), so another worker repeats the job without waiting 30 s (refines ADR-0020's shutdown).
- **expectedStatus.** The contract pattern `^(2xx|2[0-9]{2}|[45][0-9]{2})$` rejects 1xx and 3xx with 400 (request validator, ADR-0017); `monitors.ValidateExpectedStatus` applies the same rule with 422 for callers that bypass the contract.
- **One address policy.** `safehttp.Allowed(netip.Addr)` is the built-in rule set (without `MAIAK_BLOCKED_CIDRS`, which the create-time validator does not know); the monitors validator uses it plus its own name rules (`localhost`, `*.localhost`, `*.internal`, `*.local`, `*.svc`, single labels, non-canonical IPv4) and its explicit rejection of IPv4-mapped literals. IPv6 outside `2000::/3` is denied by default.
- **Configuration.** `config.Server` has no encryption fields; `config.Encryption` (keys) is embedded by `API` and `Jobs` only, `config.Guard` (`MAIAK_BLOCKED_CIDRS`) by `API`, `Worker` and `Jobs`. `LoadJobs` is new; `LoadWorker` no longer requires the keys (L-042).
- **Errors.** `scheduler.ErrNoLocation` maps to 503 `urn:maiak:problem:unavailable`; every `{monitorId}` operation documents 400 for a malformed id.

## Consequences
- The revoke is not expand/contract safe for one request type: an old api pod that resumes a monitor during the rollout gets a permission error (L-070).
- One more definer function in the schema-lint allow-list (grantee `maiak_api`), with an integration test of the tenant filter.
- The chart must stop giving the worker the encryption keys (L-042, D-03).

## Alternatives considered
- Column grants on the lease fields for `maiak_api`: ADR-0018 keeps lease fields worker-only.
- A mode argument (`pause` versus `resume`) on the reset function: the only difference would be the counters, which nobody can observe while paused.
- Recording a cancelled check with an error kind `cancelled`: it would count as a failure in uptime and could open an incident.
