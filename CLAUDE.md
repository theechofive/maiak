# Maiak: instructions for AI agents and contributors

Maiak is a multi-tenant uptime-monitoring SaaS starter: external HTTPS checks, incidents, Slack alerts, private dashboard. Read `docs/architecture/overview.md` and the ADRs in `docs/adr/` before changing structure. Read `docs/LEDGER.md` before starting work and update it as you go.

## Repository layout
- `backend/` Go modular monolith. One module, several processes: `cmd/api`, `cmd/worker`, `cmd/jobs`, `cmd/migrate`, `cmd/maiakctl` (later `cmd/probe`, `cmd/agent`).
- `api/openapi.yaml` the API contract and source of truth (spec-first). Server code and the TypeScript client are generated from it.
- `web/` React SPA (stack not chosen yet, see ledger L-015).
- `deploy/helm/maiak/` our own Helm chart (not the company `helm-app`).
- `docs/` architecture, ADRs, ledger, runbooks.

## Architecture rules (see ADRs for the why)
1. Package-by-feature under `backend/internal/<domain>`. Each table has ONE owning module; other modules write to it only through the owner's service. Read-only cross-table queries for the dashboard live in `internal/queries`.
2. Interfaces only at real seams (`Prober`, `Notifier`, `ResultStore`, `JobSource`/`ResultSink`), declared on the consumer side. No interface for the sake of it, no DI framework: wire dependencies manually in `main.go`.
3. All tenant data access goes through `db.WithTenantTx` (sets `app.tenant_id` with `SET LOCAL`, RLS enforced). Never use the pgx pool directly for tenant data (lint rule). Every tenant-scoped table has `tenant_id`; references use composite `(tenant_id, id)` keys.
4. IDs are UUIDv7 generated in the app. Timestamps are `timestamptz` UTC. Enums are `text` + `CHECK`. Secrets stored in the DB are encrypted with AES-256-GCM (`key_id` for rotation).
5. State change + notification are ONE transaction: incident transitions write `notification_deliveries` (outbox) in the same tx. Delivery happens later in `jobs`.
6. The API is spec-first: change `api/openapi.yaml` first, regenerate, then implement. Breaking changes need an ADR. Errors are RFC 9457 `problem+json`, mapped in one place.
7. SSRF defence is in code (the cluster's NetworkPolicy is NOT enforced): connect-time IP validation, blocked ranges, redirect re-validation, no proxy from env, no response body returned to users. Never weaken it without an ADR.
8. Runtime DB roles are never owners and never `BYPASSRLS`. The worker claims jobs through `SECURITY DEFINER` functions.

## Conventions
- Config from environment variables (`MAIAK_*`), validated at startup, fail fast. Never log secret values.
- Logs: `slog` JSON with `request_id` and `tenant_id`. Metrics: Prometheus on the admin port (`:9090`), not routed externally.
- Tests: unit tests for pure logic (TDD for the incident state machine), integration tests against real Postgres in CI (`TEST_DATABASE_URL`, skipped when unset), handler tests against the generated server, contract lint + `oasdiff`.
- Work in small vertical slices, each one deployable to the cluster. No local dev environment is assumed: CI is the safety net.
- Code comments: only the non-obvious why. Documentation and code are written in English.

## Deployment (GitOps, `klr-platform` cluster)
- ArgoCD deploys; do not `kubectl apply` to change cluster state. CI builds multi-arch images (`amd64` + `arm64`) to `ghcr.io/theechofive/maiak-*` and commits the SHA tag into `deploy/helm/maiak/values-platform.yaml`.
- ALWAYS pass an explicit `--context` to kubectl and `--profile` to the AWS CLI. The developer's default context may point at a production cluster. Never run anything mutating against production.
- Secrets come from AWS Secrets Manager through ExternalSecret (`platform/maiak/...`). Never put secrets in Git, values files, logs or docs.
- Internal hostname: `maiak.klr-ops.net` on the internal Envoy Gateway. `/api` goes to the api, `/` to the web.

## Ledger protocol (mandatory)
`docs/LEDGER.md` is the single register of deferred work, problems and risks.
- Found a problem, took a shortcut, deferred something, or hit a limitation? Add an entry in the same PR with a stable ID (`L-nnn` for deferred/problem, `R-nnn` for risks), context and a revisit trigger.
- Every `TODO` in code must reference an ID: `TODO(L-012): ...`. CI fails on bare TODOs (`scripts/check-todos.sh`).
- Closing an entry: set status to `done` and link the PR or ADR. Never delete entries.

## Definition of done for a slice
Lint, tests, generated code up to date, `helm lint` green, deployed to `klr-platform`, docs and ledger updated, ADR written for any architectural decision.
