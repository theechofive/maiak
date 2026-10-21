# Runbook: database migrations

Rules are in [ADR-0013](../adr/0013-migrations.md). This page is the practical side.

## Adding a migration
1. Create `backend/migrations/<UTC YYYYMMDDHHMMSS>_<snake_case_description>.sql` (use the current UTC time).
2. Start with `-- +goose Up`, then `SET LOCAL lock_timeout = '5s';`. No `Down` section.
3. Write it so the previous application version still works (expand/contract). Add nullable or defaulted columns; do not drop or rename in the same release.
4. New tenant-scoped tables need: `tenant_id uuid not null`, `ENABLE` and `FORCE ROW LEVEL SECURITY`, a policy on `current_setting('app.tenant_id', true)::uuid`, composite foreign keys `(tenant_id, id)`. Default privileges already grant the runtime roles; add explicit grants only when you need less than the default (for example insert-only).
5. Large indexes: a separate migration with `-- +goose NO TRANSACTION` and `CREATE INDEX CONCURRENTLY`.
6. Update the queries in the owning module and run `make generate`. Commit the generated code.
7. A new `SECURITY DEFINER` function also needs an entry in `definerAllowList` (`backend/migrations/integration_test.go`) with the one runtime role allowed to execute it, `SET search_path = public, pg_temp`, `REVOKE ALL ... FROM PUBLIC` and a `GRANT EXECUTE` to that role only. A new non-isolation policy needs an entry in `extraPolicies`. Both are reviewed changes.
8. Changing the signature of a definer function the previous release calls breaks that release during the rollout (L-090): add the new function next to the old one and drop the old one a release later. Changing only its body with `CREATE OR REPLACE` (same arguments and result) keeps the owner and the grants and is rollout-safe, as the heartbeat migration does for `claim_due_monitors`.
9. CI applies all migrations from scratch on Postgres 15, runs the schema-lint test and checks that generated code did not drift.

## Current migrations
| File | Content |
|---|---|
| `20261020000100_init_tenancy_auth.sql` | Default privileges, `tenants`, `users`, `memberships`, `auth_identities`, `sessions`, `audit_log`, RLS and grants, `auth_lookup_session` |
| `20261020120000_monitors_locations.sql` | `probe_locations` (seeds `home-eu-central-1`), `monitors`, `monitor_locations`, `monitor_location_state`, `monitor_state` |
| `20261020130000_results.sql` | `check_results` (monthly partitions), `result_rollups_hourly`, `ensure_partitions` (called once here), `claim_due_monitors(n)`, `compute_rollups`, `drop_expired_partitions`, `purge_expired_sessions` |
| `20261020140000_incidents_outbox.sql` | `incidents`, `incident_monitors`, `notification_channels`, `monitor_channels`, `notification_deliveries`, `claim_deliveries`, `finish_delivery(id, status, error, retry_at)` |
| `20261020160000_api_runtime_grants.sql` | B-19: revokes table-wide `UPDATE` on `monitor_location_state` and `monitor_state` from `maiak_api`, column grant on `monitor_state`, `reset_monitor_runtime` (L-070) |
| `20261020170000_outbox_stats.sql` | `outbox_pending` |
| `20261020180000_review_fixes_b10_b12.sql` | B-20: `incidents.monitor_id` and the one-open-incident index, `claim_due_monitors(n, p_location)`, `notification_deliveries.claim_token`, new `claim_deliveries` and `finish_delivery(id, token, ...)`, `compute_rollups` under an advisory lock, `drop_expired_partitions` replaced by `list_droppable_partitions` / `detach_expired_partition` / `drop_detached_partition` (L-090) |
| `20261020190000_rollup_watermark_review_fixes_b21.sql` | B-21: `rollup_state` and `compute_rollups_catch_up` (ADR-0090; `compute_rollups(from, to)` kept), `reset_monitor_runtime` skips soft-deleted monitors |
| `20261021090000_plans.sql` | P-01: `plans` (global, read-only for runtime roles; seeds `free` and `internal`), `tenants.plan_key` (default `free`, FK to `plans`), the `default` tenant moved to `internal` (ADR-0093) |
| `20261021120000_registration_login_tenant.sql` | P-02: `auth_login_tenant(user_id)` and the `owner_membership_lookup` policy (SELECT, `maiak_owner` only) on `memberships`, so a login binds to the user's own tenant (ADR-0009 amendment 2026-10-21) |
| `20261021150000_heartbeat_monitors.sql` | P-03: monitor type `heartbeat` (the MVP interval and timeout constraints now bind `http` only, `monitors_heartbeat_check`), `heartbeat_tokens` (token hash, ping and evaluation times) with the `owner_heartbeat_read` / `owner_heartbeat_ping` policies, `heartbeat_ping(hash)` (`maiak_api`), `list_due_heartbeats(n)` (`maiak_worker`); new bodies, same signatures, for `claim_due_monitors` (http only) and `reset_monitor_runtime` (heartbeats stay parked at `next_run_at = infinity`) (ADR-0094) |
| `20261021170000_review_fixes_p07.sql` | P-07: `maiak_api` keeps only `UPDATE (updated_at)` on `tenants` (INSERT revoked later, L-210), `tenants_limits_is_object` check, `auth_create_tenant(id, slug, name)` (plan `internal` only for `default`), `auth_count_other_memberships(user, tenant)` and `auth_revoke_user_sessions(user)` for `BootstrapAdmin`, new body of `auth_login_tenant` preferring `default` (ADR-0095) |
| `20261021180000_heartbeat_hardening.sql` | P-08 (ADR-0096): `monitors_heartbeat_check` requires `graceSeconds` as a plain integer (`^[0-9]+$`, 0-3600); existing `60.0` spellings are rewritten per tenant, any other invalid heartbeat fails the migration with its ids; `heartbeat_ping` locks the row and stores `greatest(last_ping_at, clock_timestamp())`; `list_due_heartbeats` casts the grace through `numeric`. Same signatures, owners and grants. |

`claim_due_monitors(n)`, `finish_delivery(id, status, error, retry_at)` and `drop_expired_partitions()` no longer exist; the [data model](../architecture/data-model.md) lists the current functions.

## What the tests enforce
- `TestMigrationFiles` (unit, no database): name `YYYYMMDDHHMMSS_snake_case.sql` with a valid UTC timestamp and no duplicate, no `+goose Down`, the first statement after `+goose Up` is `SET LOCAL lock_timeout = '5s';` (`SET lock_timeout = '5s';` in a `NO TRANSACTION` migration). CI also checks file naming in the repo-hygiene job.
- `TestIntegrationMigrateFromScratchAndRerun`: all migrations apply to an empty database created by `dbbootstrap` and a second run is a no-op.
- `TestIntegrationSchemaLint` (schema lint), on the migrated database:
  - every table with `tenant_id` has RLS enabled and forced and a permissive `tenant_isolation`-shaped policy (all commands, PUBLIC or runtime roles, `USING` and `WITH CHECK` on `app.tenant_id`);
  - any other permissive policy on a tenant table must be in `extraPolicies` with exactly its command and roles (today only the `owner_*` policies for `maiak_owner` used by definer functions);
  - runtime roles own nothing, are not superusers, do not bypass RLS and are not members of `maiak_owner`;
  - every `SECURITY DEFINER` function is in `definerAllowList`, owned by `maiak_owner`, has a pinned `search_path`, is not executable by PUBLIC and is executable only by its allow-listed role (directly or through a group).
  The test also proves each rule fails on a deliberately broken object.
- Further integration tests in `backend/migrations` cover the results, outbox, runtime grants, monitors, review-fix, plans and heartbeat migrations (heartbeat constraints and grants, `heartbeat_ping` and `list_due_heartbeats`, workers never claiming a heartbeat, partitions, rollups, retention, claims, fencing, grants of `maiak_api`, plans read-only for runtime roles and the `default` tenant moved to `internal` when the plans migration runs over existing data, `auth_login_tenant` choosing the oldest live membership). A test that replays an older migration must roll back the later ones too: goose refuses to apply a missing older version (see `plans_integration_test.go`).

## How it runs in the cluster
ArgoCD sync order: bootstrap hook (wave -3, when enabled), migrate Job (wave -1), workloads (wave 0). A failed migration stops the sync; old pods keep running. Look at the Job logs: `kubectl --context <klr-platform context> -n maiak logs job/maiak-migrate`.

## Fixing a bad migration
Never edit an applied file. Write a new forward migration that corrects it. For an emergency, stop the rollout, fix forward, push. If a migration is half-applied (non-transactional), check `goose_db_version` and the catalog before writing the fix.

## Rollback of the application
Roll the image tag back in `deploy/helm/maiak/values-platform.yaml`. This is safe only because migrations are backward-compatible with the previous version; the schema is not rolled back.
