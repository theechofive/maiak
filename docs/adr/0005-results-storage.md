# ADR-0005: Partitioned results in Postgres with hourly rollups

- Status: Accepted
- Date: 2026-10-20

## Context
Results are the highest-volume data (144 M rows per day at 100 000 monitors with one location). The product needs recent history, uptime percentages and latency charts, not infinite raw history.

## Decision
`check_results` is partitioned monthly by `checked_at`; raw retention is 30 days (configurable per tenant or plan) implemented as `DROP PARTITION`. `jobs` creates partitions ahead and computes `result_rollups_hourly`, kept 13 months. The dashboard reads rollups for long ranges and raw rows only for recent ones. The sink sits behind the `ResultStore` seam. Scheduling uses a Postgres-backed queue with `SELECT ... FOR UPDATE SKIP LOCKED` on `monitor_location_state`, no Redis/NATS/Kafka.

## Consequences
- No extra infrastructure for the MVP; one datastore.
- State tables are hot (frequent updates): fillfactor and autovacuum tuning needed.
- At scale stage 2 the raw store moves to ClickHouse behind the same seam (ADR-0012, L-013).

## Alternatives considered
- TimescaleDB or ClickHouse from day one: extra operations for a workload that Postgres handles at the MVP scale.
- Row-by-row deletes for retention: slow and bloats.
