# ADR-0003: Modular monolith deployed as several processes

- Status: Accepted
- Date: 2026-10-20

## Context
One developer, small product, tight cluster memory. The most valuable guarantee (incident transition and alert recorded atomically) relies on one database transaction. Scale bottlenecks are in the data layer, not in code boundaries.

## Decision
One Go module, packages by feature under `backend/internal`, interfaces only at real seams (`Prober`, `Notifier`, `ResultStore`, `JobSource`/`ResultSink`), declared on the consumer side. Dependencies wired manually in `main.go`. Deployed as separate processes from one image: `api`, `worker`, `jobs` (and later `probe`, `agent`). Each table has one owning module; the dashboard reads through `internal/queries`. The incident state machine runs synchronously in the result-write transaction.

## Consequences
- Operations stay simple: one chart, one CI, one set of roles.
- Extraction to services stays possible at the seams when there is a measured need (independent scaling, different failure domain, separate team).
- Needs discipline: module boundaries are enforced by review and lint, not by the network.

## Alternatives considered
- Microservices: distributed transactions (sagas) for the incident+outbox guarantee, per-service charts/CI/roles, harder debugging. Rejected for now.
- Classic layered by technical type: spreads each feature across directories.
- Full clean/hexagonal/DDD everywhere: ceremony for a CRUD-heavy product.
