# ADR-0018: Decisions from the review of B-05..B-09

- Status: Accepted
- Date: 2026-10-20

## Context
An independent review found no SSRF bypass but several consistency gaps between the create-time validation, the connect-time guard, the scheduler lease and the database grants.

## Decision
- `expectedStatus` accepts `2xx`, an exact code 200-299, or an exact code 400-599. 1xx and 3xx are rejected with 422 because redirects are followed and 1xx is consumed by the HTTP client.
- One source of truth for blocked addresses: the create-time validator calls the exported `safehttp.Allowed`. IPv6 is default-deny: only `2000::/3` is allowed, minus the blocklist.
- `MAIAK_BLOCKED_CIDRS` is read by api, worker and jobs so every use of the guarded client shares the operator's ranges.
- A cancelled parent context is not a failed check: the prober returns a distinct cancelled marker and the worker discards it.
- Pause clears the lease token, which fences any in-flight result. Changing a monitor's URL resets its state like resume (status unknown, counters zero, open incident resolved with reason `recovered` is NOT used; reason `url_changed` is added to the incident resolve reasons by B-11).
- `maiak_api` gets column-level UPDATE grants on the scheduler and state tables; lease fields are writable only by the worker role, except the reset performed on resume, pause and URL change, which goes through a `SECURITY DEFINER` function `reset_monitor_runtime(monitor_id)` callable by `maiak_api`.
- Raw errors from the guarded client contain resolver or peer addresses; they must not be logged or stored. Only error kinds are persisted.

## Consequences
- A new migration and a new definer function (added to the schema-lint allow-list with grantee `maiak_api`).
- The integration tests of the monitors module must be run against Postgres in CI before B-19 is considered done (the reviewer's local run skipped them).
