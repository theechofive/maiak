# Runbook: database migrations

Rules are in [ADR-0013](../adr/0013-migrations.md). This page is the practical side.

## Adding a migration
1. Create `backend/migrations/<UTC YYYYMMDDHHMMSS>_<snake_case_description>.sql` (use the current UTC time).
2. Start with `-- +goose Up`, then `SET LOCAL lock_timeout = '5s';`. No `Down` section.
3. Write it so the previous application version still works (expand/contract). Add nullable or defaulted columns; do not drop or rename in the same release.
4. New tenant-scoped tables need: `tenant_id uuid not null`, `ENABLE` and `FORCE ROW LEVEL SECURITY`, a policy on `current_setting('app.tenant_id', true)::uuid`, composite foreign keys `(tenant_id, id)`. Default privileges already grant the runtime roles; add explicit grants only when you need less than the default (for example insert-only).
5. Large indexes: a separate migration with `-- +goose NO TRANSACTION` and `CREATE INDEX CONCURRENTLY`.
6. Update the queries in the owning module and run `make generate`. Commit the generated code.
7. CI applies all migrations from scratch on Postgres 15, runs the schema-lint test and checks that generated code did not drift.

## How it runs in the cluster
ArgoCD sync order: bootstrap hook (wave -3, when enabled), migrate Job (wave -1), workloads (wave 0). A failed migration stops the sync; old pods keep running. Look at the Job logs: `kubectl --context <klr-platform context> -n maiak logs job/maiak-migrate`.

## Fixing a bad migration
Never edit an applied file. Write a new forward migration that corrects it. For an emergency, stop the rollout, fix forward, push. If a migration is half-applied (non-transactional), check `goose_db_version` and the catalog before writing the fix.

## Rollback of the application
Roll the image tag back in `deploy/helm/maiak/values-platform.yaml`. This is safe only because migrations are backward-compatible with the previous version; the schema is not rolled back.
