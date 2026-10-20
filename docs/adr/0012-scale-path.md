# ADR-0012: Scale path to about 10 000 tenants

- Status: Accepted (analysis, not a benchmark)
- Date: 2026-10-20

## Context
The product should be able to grow to roughly 10 000 tenants over several years without a rewrite.

## Decision
Assumptions: 10 monitors per tenant, 60 s interval. 100 000 monitors give about 1 700 checks/s and 144 M result rows per day (about 4.3 B rows over 30 days, 0.6-0.8 TB with indexes). Several locations multiply this.

| Stage | Scale | Changes |
|---|---|---|
| 0, MVP | up to about 100 tenants, about 80 checks/s | none; shared RDS |
| 1 | about 1 000 tenants, up to 800 checks/s | dedicated Multi-AZ RDS (L-009), 2-4 workers, autovacuum tuning of state tables |
| 2 | about 10 000 tenants, about 1 700 checks/s | shorter raw retention (7-14 days) or ClickHouse for `check_results` (L-013), read replica for the dashboard, multi-region probes, batched ingest for agents |
| 3 | 100 000+ tenants | sharded in-memory scheduler instead of the Postgres queue, Kafka between probes and storage, cells with tenant-to-cell routing |

The architecture keeps these moves local: `tenant_id` everywhere and composite keys allow tenant sharding, the results sink sits behind `ResultStore`, scheduling behind `JobSource`, executors behind the probe protocol (ADR-0006).

Cheap measures taken now: workers do not read response bodies by default (64 KB cap only for keyword checks), raw retention is configurable, per-tenant and per-host concurrency limits, and a load test with 100 000 synthetic monitors before open registration (L-010).

Non-technical scale risks tracked in the ledger: mass false-positive alerts (L-012), abuse of probes against third parties (L-011), traffic spikes on public status pages (L-004), network egress cost (L-017).

## Consequences
- Throughput numbers are estimates and must be validated (L-010).
- Worker count is driven by number of locations x 2 for availability, not by load.

## Alternatives considered
- ClickHouse and Kafka from day one: operations cost with no MVP benefit.
