# Maiak

Multi-tenant uptime monitoring, built as a SaaS starter. External HTTPS checks, incidents, Slack alerts and a private dashboard. A public status page, an in-cluster agent for internal services and regional probes are planned.

Status: early development (walking skeleton). See `docs/LEDGER.md` for what is deferred.

## Layout
| Path | What |
|---|---|
| `backend/` | Go modular monolith (api, worker, jobs, migrate, maiakctl) |
| `api/openapi.yaml` | API contract (spec-first) |
| `web/` | React SPA (stack TBD) |
| `deploy/helm/maiak/` | Helm chart |
| `docs/` | Architecture, ADRs, ledger |

## Read first
1. `docs/PROJECT.md` (goal, MVP scope, decisions, plan, ownership)
2. `docs/architecture/overview.md`
3. `docs/adr/`
4. `docs/LEDGER.md`
5. `CLAUDE.md` (conventions, also for AI agents)
