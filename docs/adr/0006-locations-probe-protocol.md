# ADR-0006: Locations and the pull/push probe protocol

- Status: Accepted
- Date: 2026-10-20

## Context
Geo checks need executors in several regions and providers. Client clusters need an agent. The executor is the most exposed component because it fetches arbitrary URLs.

## Decision
A location is a (region x network/provider) pair. Each (monitor, location) runs with the full interval; the number of locations per monitor is limited by plan (MVP: one). Remote executors never connect to the database: they call the api (`claim`, `results`) over HTTPS with a per-location token and initiate all connections (no inbound ports). The `agent` for client clusters uses the same protocol. One executor core, three entry points: `cmd/worker` (home, DB mode), `cmd/probe` (remote, minimal binary without DB code or secrets), `cmd/agent` (client cluster). The seams `JobSource` and `ResultSink` have a local (DB) and a remote (HTTPS) implementation. Monitor status is aggregated from per-location state by a quorum rule.

## Consequences
- A compromised remote probe yields only its location token.
- Result volume and load scale with locations per monitor.
- A public entry for `/probe` is needed in the cluster (R-005, L-006).
- False-positive protection (canaries, second-location confirmation) becomes necessary (L-012).

## Alternatives considered
- Remote workers connecting directly to Postgres: exposes the database and credentials to untrusted environments.
- Splitting the interval across locations: lower cost but weaker detection per location.
