# MVP task lists

- [Backend tasks](backend.md)
- [Frontend tasks](frontend.md)
- [Deploy tasks](deploy.md)

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
| `GET /monitors` | list with current status, uptime 24h, last response time, last check; plus summary counts (up, down, paused) |
| `POST /monitors` | create |
| `GET /monitors/{id}` | one monitor with current status, TLS expiry, ongoing incident |
| `PATCH /monitors/{id}` | edit |
| `DELETE /monitors/{id}` | soft delete |
| `POST /monitors/{id}/pause`, `POST /monitors/{id}/resume` | pause and resume |
| `GET /monitors/{id}/stats?range=24h\|7d\|30d` | uptime %, average and p95 response time, latency series (buckets) |
| `GET /monitors/{id}/checks?limit=` | recent checks |
| `GET /monitors/{id}/incidents` | incidents of the monitor |
| `GET /settings/slack`, `PUT /settings/slack`, `POST /settings/slack/test` | Slack webhook configuration (write-only secret) and test message |

Monitor fields: `name`, `url` (https), `intervalSeconds` (60 only in the MVP), `timeoutSeconds` (10 only), `expectedStatus` (`2xx` or an exact code), `failureThreshold` (1-3, default 2), `notifySlack` (bool), `paused`.
