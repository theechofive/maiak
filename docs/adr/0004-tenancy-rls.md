# ADR-0004: Shared schema with tenant_id and Row-Level Security

- Status: Accepted
- Date: 2026-10-20

## Context
Tenants must be isolated. The scheduler needs to pick due work across all tenants efficiently. We want to scale to thousands of tenants without per-tenant operations.

## Decision
One schema, `tenant_id` on every tenant-scoped table, RLS forced with a policy on `current_setting('app.tenant_id')`. All tenant data access goes through `db.WithTenantTx` (`SET LOCAL`). Composite `(tenant_id, id)` foreign keys. Runtime roles `maiak_api` and `maiak_worker` are not owners and not `BYPASSRLS`; the worker picks work via `SECURITY DEFINER` functions. Users are global; sessions are looked up by token hash through a `SECURITY DEFINER` function because the tenant is not known yet.

## Consequences
- One migration for all tenants; cheap; tenant-based sharding stays possible later (all tables carry `tenant_id`).
- Tenants share DB resources (noisy neighbour): per-tenant quotas and limits are required.
- Restoring a single tenant from a backup is harder.
- `SECURITY DEFINER` functions must pin `search_path` and be reviewed carefully.

## Alternatives considered
- Schema per tenant: migrations N times, `search_path` pitfalls, scheduler must scan schemas.
- Database per tenant: strongest isolation, heavy operations; for enterprise only.
