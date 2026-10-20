# ADR-0041: Dashboard read models (B-15)

- Status: Accepted
- Date: 2026-10-20

## Context
The dashboard needs list, detail, statistics, recent checks and incidents. ADR-0005 and ADR-0016 say ranges up to 24 h come from raw rows and longer ones from hourly rollups, rollups cover closed hours only, and the list must not scan raw results. Rollups are written by `jobs` (B-14) and may lag behind.

## Decision
- **Package `internal/queries`**, read-only, one tenant transaction per request. The SQL is hand-written with pgx (as `scheduler.Claim`), not sqlc: the window arithmetic, laterals and `width_bucket` over arrays type poorly in sqlc, and nothing here is reused by a writer.
- **Rollups plus raw for what is not rolled up.** A window `[from, to)` counts the partial first hour from raw rows, the whole hours from rollups up to the newest rollup in the window, and every later hour (the current one and any hour `jobs` has not rolled up yet) from raw rows. Raw rows are only ever read through the primary key `(monitor_id, checked_at, ...)` for bounded ranges. If `jobs` stops, the raw part grows up to the window; the result stays correct.
- **Uptime** is ok/total over checks that ran; null without checks. 24 h, 7 d and 30 d use the same exact window rule. The list computes 24 h for all monitors in one query.
- **Paused monitors** report `status: unknown` with `paused: true`; the summary counts them only as paused.
- **lastResponseMs** is the latency of the newest result, found by the primary key from `monitor_state.last_checked_at - 1 h` (prunes old partitions); null when the newest check got no response.
- **Stats.** Latency statistics cover checks that got a response (the population of the rollup histogram, ADR-0021). `24h`: raw rows, exact average and `percentile_cont(0.95)`, 15-minute buckets (97 points). `7d`: hourly buckets (169), `30d`: 6-hour buckets (121), from rollups merged with raw rows of the hours not rolled up, which are bucketed with the same edges (`width_bucket` with edge+1 thresholds). p95 for 7 d and 30 d is the inclusive upper edge of the histogram bucket that contains the ceil(0.95 n)-th latency, so it overstates the true value by at most one bucket width; the overflow bucket reports 10000 ms. 7 d and 30 d start at the hour of `now - range`. Every bucket of the range is returned, empty ones with null values.
- **Checks**: newest first, default 50, at most 200 (the contract maximum was raised from 100, a non-breaking change). **Incidents**: newest first, at most 100, `resolvedAt` null while ongoing; a missing `reason` reads `unknown`.
- Mutating monitor endpoints read the monitor back through the read model, so every `MonitorDetail` response has uptime, ongoing incident and last response time.

## Consequences
- An hour that has no rollup while a later hour has one is not counted (compute_rollups runs in order, so this needs a failed or partial run) (L-071).
- p95 for 7 d and 30 d is coarse (L-072); results of a monitor's previous URL still count in its uptime and statistics (L-071).
- The list issues per-monitor index lookups; at 50 monitors this is cheap, at scale it needs measuring (R-050).

## Alternatives considered
- Rollups only for closed hours and raw only for the current hour: wrong numbers whenever `jobs` lags.
- p95 by linear interpolation inside the histogram bucket: looks precise without being so; the upper edge is an honest bound.
- Separate list endpoints for summary and items: more requests for the same data.
