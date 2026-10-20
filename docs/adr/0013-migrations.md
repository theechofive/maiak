# ADR-0013: Database migrations: embedded goose, forward-only, expand/contract

- Status: Accepted
- Date: 2026-10-20

## Context
The schema must evolve safely without a local development environment. A migration runs before new pods start, so the previous application version is still serving traffic while it runs.

## Decision
- Tool: goose. SQL files in `backend/migrations`, embedded into `/app/migrate` (`go:embed`), so schema and code ship in one image. Applied versions are tracked in `goose_db_version`.
- Run by `cmd/migrate` as the `maiak_owner` role in an ArgoCD hook Job (wave -1), after the database bootstrap (ADR-0014) and before workloads (wave 0). A failed migration stops the sync and old pods keep running. A Postgres session advisory lock prevents overlapping runs.
- Forward-only: no `Down` sections. A rollback is a new forward migration.
- Names: UTC timestamp prefix `YYYYMMDDHHMMSS_description.sql`, so parallel branches do not collide on numbers. CI checks the pattern, uniqueness and the absence of `Down`.
- Expand/contract: every migration must work with the previous application version. Add nullable or defaulted columns first; drop or rename only in a later release; any destructive change is recorded in the ledger.
- Lock safety: `SET LOCAL lock_timeout = '5s'` at the top of each migration; indexes on large tables with `CREATE INDEX CONCURRENTLY` in a `NO TRANSACTION` migration.
- Roles and the database are NOT created by migrations (ADR-0014). Migrations only grant. The first migration sets default privileges for `maiak_owner` so new objects get the right grants.
- Schema-lint test in CI: every table with a `tenant_id` column must have RLS enabled and forced and at least one policy; runtime roles must not own objects, must not be superusers and must not have `BYPASSRLS`.
- Data seeding that depends on secrets (admin user, default tenant) is done by the application at startup, idempotently, not by migrations.

## Consequences
- No automatic undo; mistakes are fixed forward, and dangerous changes need two releases.
- A forgotten RLS policy fails CI instead of leaking data.
- The schema is only as fresh as the image that carries it.

## Alternatives considered
- Up/down migrations: `Down` is rarely safe once data has changed and doubles the maintenance.
- Sequential numbers: collisions between parallel branches (including AI agents).
- Application running migrations at startup in every pod: races, privilege concerns, no ordering with other hooks.
