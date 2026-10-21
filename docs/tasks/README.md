# MVP task lists

- [Backend tasks](backend.md)
- [Frontend tasks](frontend.md)
- [Deploy tasks](deploy.md)
- [Phase 2 tasks](phase2.md): plans and quotas, registration, watchdog, admin panel

## Conventions
- IDs: `B-nn` backend, `F-nn` frontend, `D-nn` deploy. Statuses: `todo`, `doing`, `done`, `blocked`.
- One commit per task, short message in the form `B-07: SSRF guard and HTTPS prober`, no push until the owner decides.
- A task is done when: its acceptance criteria hold, lint and tests pass, generated code is current, docs and ledger are updated, and the work was verified by a pass separate from the author.
- Decisions made while doing a task go into an ADR (or the ledger when deferred) in the same commit.
- Anything found but not fixed goes into `docs/LEDGER.md` with a revisit trigger.

## API contract (MVP), all under `/api/v1`
| Method and path | Purpose |
|---|---|
| `GET /health` | public liveness with version |
| `POST /auth/login`, `POST /auth/logout`, `GET /auth/me` | session auth; login and `me` return the CSRF token |
| `GET /monitors` | list with current status, `statusSince`, `lastErrorKind`, `lastCheckedAt`, uptime 24h, last response time; plus summary counts (up, down, paused, unknown) |
| `POST /monitors` | create |
| `GET /monitors/{id}` | one monitor with status, `statusSince`, `lastErrorKind`, `lastCheckedAt`, TLS expiry, ongoing incident and `uptime` for 24h, 7d and 30d |
| `PATCH /monitors/{id}` | edit |
| `DELETE /monitors/{id}` | soft delete |
| `POST /monitors/{id}/pause`, `POST /monitors/{id}/resume` | pause and resume |
| `GET /monitors/{id}/stats?range=24h\|7d\|30d` | average and p95 response time and the latency series (buckets) for the range |
| `GET /monitors/{id}/checks?limit=` | recent checks |
| `GET /monitors/{id}/incidents` | incidents of the monitor |
| `GET /settings/slack`, `PUT /settings/slack`, `POST /settings/slack/test` | Slack webhook configuration (write-only secret) and test message |

Monitor fields: `name`, `url` (https), `intervalSeconds` (60 only in the MVP), `timeoutSeconds` (10 only), the URL must use `https` and port 443, `expectedStatus` (`2xx` or an exact code), `status` is read-only (`unknown`, `up`, `down`) and `paused` is a boolean, `failureThreshold` (1-3, default 2), `notifySlack` (bool), `paused`.
