# Maiak: project brief

Maiak is a multi-tenant uptime-monitoring service built as a SaaS starter. This page is the entry point: what we are building, what is in and out of the MVP, the decisions in one place, the delivery plan and who owns what. Details live in the linked documents. Anything unfinished is in the [ledger](LEDGER.md).

## Goal
Monitor the availability of websites (and later internal services in client Kubernetes clusters), alert through Slack, show current status and history in a private dashboard. Designed from day one for many clients (multi-tenant), operated first by one person.

## MVP scope
**In**
- Login of a single admin (credentials from a Secret), server-side session, CSRF protection.
- CRUD of HTTPS monitors: interval 60 s, timeout 10 s, expected status 2xx, TLS expiry warning 14 days.
- Worker that runs the checks from one location (`klr-platform`, eu-central-1).
- History: raw results 30 days, hourly rollups 13 months.
- Incidents: a monitor is down after 2 consecutive failures; an incident opens and resolves automatically.
- Slack notifications (Incoming Webhook) for opened and resolved incidents.
- Private dashboard: monitor list with status, monitor details (uptime %, latency chart, incidents). Status refresh by polling every 15-30 s.

**Out of the MVP** (all tracked in the ledger): public status page (L-004), in-cluster agent (L-005), regional probes (L-006), multiple users and OAuth (L-007), billing and plans (L-008), TCP/DNS/Kubernetes checks and SSE (L-014), self-monitoring of Maiak (L-003), SSRF policy details (L-001).

## Decisions at a glance
| Topic | Decision | Where |
|---|---|---|
| Product model | Hosted multi-tenant SaaS; agent for internal services later | [ADR-0001](adr/0001-product-model.md) |
| Repository | One monorepo `maiak` (`backend/`, `web/`, `api/`, `deploy/`, `docs/`) | [ADR-0002](adr/0002-monorepo.md) |
| Code architecture | Modular monolith, several processes (`api`, `worker`, `jobs`), interfaces only at seams | [ADR-0003](adr/0003-modular-monolith.md) |
| Tenancy | Shared schema, `tenant_id`, forced RLS, composite keys | [ADR-0004](adr/0004-tenancy-rls.md) |
| Results storage | Monthly partitions, 30 days raw, hourly rollups, Postgres queue | [ADR-0005](adr/0005-results-storage.md) |
| Locations | Pull/push probe protocol, worker/probe/agent share one core | [ADR-0006](adr/0006-locations-probe-protocol.md) |
| Incidents and alerts | Quorum state machine, transactional outbox | [ADR-0007](adr/0007-incidents-outbox.md) |
| API | REST + OpenAPI spec-first, one origin (`/api` and `/`), polling | [ADR-0008](adr/0008-api-contract.md) |
| Auth | One admin from a Secret, real `users` table | [ADR-0009](adr/0009-auth-admin-users.md) |
| Deployment | `klr-platform`, own Helm chart, ArgoCD, ghcr multi-arch, ExternalSecret | [ADR-0010](adr/0010-deployment.md) |
| SSRF | Defence in code (NetworkPolicy is not enforced) | [ADR-0011](adr/0011-ssrf-defence.md) |
| Scale path | Stages up to about 10 000 tenants | [ADR-0012](adr/0012-scale-path.md) |
| Migrations | goose, embedded, forward-only, expand/contract, RLS schema-lint | [ADR-0013](adr/0013-migrations.md) |
| DB bootstrap | Automatic hook Job, enabled for the first deploy | [ADR-0014](adr/0014-db-bootstrap-automation.md) |

Stack: Go (`chi`, `pgx`, `sqlc`, `goose`, `oapi-codegen`, `slog`), PostgreSQL 15 (RDS, database `maiak` on the shared instance `grafana` for now), React + TypeScript with Vite for the web app ([ADR-0015](adr/0015-frontend-stack.md)), Helm, ArgoCD, GitHub Actions, `ghcr.io/theechofive/maiak-*`.

Environment: a single environment on `klr-platform` (EKS, arm64), internal host `maiak.klr-ops.net`.

## Task lists
MVP work is tracked in [tasks/backend.md](tasks/backend.md), [tasks/frontend.md](tasks/frontend.md) and [tasks/deploy.md](tasks/deploy.md). The frontend is minimal React + Vite ([ADR-0015](adr/0015-frontend-stack.md)).

## Quality approach
Unit tests for pure logic (incident state machine first, written test-first); integration tests against real Postgres in CI (migrations from scratch, RLS isolation, schema-lint); handler tests against the generated server; OpenAPI lint and `oasdiff` for breaking changes; a smoke test after deploy. There is no local development environment: CI is the safety net. Every slice is deployed to the cluster.

## Delivery plan (vertical slices)
| # | Slice | Status | Verified (as of 2026-10-20) |
|---|---|---|---|
| 1 | Walking skeleton: monorepo, CI, image, Helm, ArgoCD, `/api/v1/health`, database bootstrap and migrations, admin login | implemented (B-01..B-06, D-01..D-06) | locally only: Go tests against PostgreSQL 16, chart lint/template/kubeconform. Not verified: CI has never run on this work, no image has been built by the release pipeline (L-102, L-023), nothing deployed (blocked by L-002; L-022, L-103, R-009) |
| 2 | Monitors CRUD, locations model | implemented (B-07, B-08, B-19) | locally only: integration tests on PostgreSQL 16 (CI uses 15, R-011) |
| 3 | Worker: scheduler, HTTPS prober with SSRF guard, results, rollups, partitions | implemented (B-09..B-12, B-20) | locally only, including the end-to-end test through the real guard (B-16, `make test-e2e`); never against real targets from the cluster egress |
| 4 | Incidents, outbox, Slack delivery in `jobs` | implemented (B-11, B-13, B-14, B-20) | locally only, against a fake Slack over TLS; never posted to a real Slack webhook |
| 5 | Dashboard (web) and read API | implemented (B-15, F-01..F-09, F-11, F-12); production packaging F-10 in progress | read API by integration tests; web only in jsdom with mocked fetch, never in a browser against the real api (L-052); web image never built (L-023) |
| 6 | Hardening and the load test before any open registration (L-010, L-011, L-012) | not started; observability groundwork done (B-17) | metrics and alert rules are not scraped or deployed (L-003) |

Each slice ends with: lint and tests green, generated code up to date, `helm lint` green, deployed to `klr-platform`, docs and ledger updated, an ADR for any architectural decision.

## Who does what
| Owner | Responsibilities |
|---|---|
| Product owner (repo owner) | The repository and GitHub settings, DNS record `maiak.klr-ops.net` in Cloudflare, the PR registering the ArgoCD Application in `devops-charts-infra`, the optional secrets `platform/maiak/admin` (login and password hash) and `platform/maiak/crypto` (only with `externalSecret` mode; by default the cluster generates both, ADR-0091), the ghcr pull-secret decision (L-002) |
| Engineering (AI agents and the owner) | Code, chart, CI, documentation, ledger. Agents work from `CLAUDE.md`; independent review is a separate pass from authoring |

## Before the first deploy (blocking)
1. Decide the ghcr pull secret (L-002).
2. Optional, only with `externalSecret` mode: create the secrets `platform/maiak/admin` (`login`, `password_hash`) and `platform/maiak/crypto` in AWS Secrets Manager. By default the cluster generates both (ADR-0091) and the admin logs in as `admin` with the one-time password from `maiak-admin-initial`.
3. Add the Cloudflare CNAME for `maiak.klr-ops.net` to the internal NLB.
4. Merge the `Application` into `devops-charts-infra`.
5. After the first successful sync, set `dbBootstrap.enabled: false` (risk R-009).

## Where things are
- Architecture: [architecture/overview.md](architecture/overview.md), [architecture/data-model.md](architecture/data-model.md)
- Runbooks: [runbooks/database-bootstrap.md](runbooks/database-bootstrap.md), [runbooks/migrations.md](runbooks/migrations.md)
- Rules for contributors and agents: [../CLAUDE.md](../CLAUDE.md)
