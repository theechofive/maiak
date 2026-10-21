# ADR-0096: Heartbeat review fixes: ping limits, in-flight bound, config check, ping time

- Status: Accepted
- Date: 2026-10-21
- Amends: ADR-0094

## Context
The independent security review of P-03 (task P-08) found that the public ping endpoint of ADR-0094 counted every ping against the client IP before the lookup and refunded known tokens afterwards. Unknown-token traffic from one address, or from the single shared bucket used when `X-Forwarded-For` is missing, could therefore answer `429` to a working Alertmanager. It also hashed and looked up any string, read the body before deciding anything and had no bound on concurrent lookups, so unauthenticated traffic could hold the api's database pool. The heartbeat config check accepted `graceSeconds: 60.0`, whose text fails the `::integer` casts in `list_due_heartbeats` and the dashboard query. `heartbeat_ping` stored `now()`, the start of its transaction, so a ping that waited for the evaluation's row lock could record a time from before the evaluation's clock, and `last_ping_at` could move backwards. Every ping of a healthy heartbeat ran the incident machine (L-202).

## Decision
- **Order of the ping handler.** (1) A token that is not exactly 43 characters of `[A-Za-z0-9_-]` (what `NewHeartbeatToken` issues) is unknown: no hash, no body read, no database work. (2) The per-token limit, one ping per second per token hash. (3) The body is drained (at most 64 KiB). (4) An in-flight slot. (5) The lookup (`heartbeat_ping`). (6) Only a token the lookup does not know, or a malformed one, is counted against the client IP: up to 30 per 5 minutes get `404`, the rest `429`. A known token never consults the per-IP limiter, so neither an exhausted IP nor the shared zero-address bucket can block it. An unknown token's per-token entry is dropped after the lookup, so unknown tokens only ever see the per-IP `429`.
- **Why the IP is counted after the lookup.** Counting before the lookup and skipping tokens found in a cache of recently accepted hashes was considered: a known token missing from the cache (after a restart, or evicted) from an exhausted address would get `429` and never enter the cache, so a flood behind a misconfigured proxy would keep it out for good. Counting after the lookup is the simplest design where a known token is never refused by the IP limit. The database work of unknown tokens is bounded by the in-flight limit instead.
- **In-flight bound.** At most 64 lookups run or wait for a connection at once per replica; a ping beyond that gets `429` with `Retry-After: 1` (`result="busy"`). 16 of the 64 slots are reserved for tokens in a bounded LRU of recently accepted hashes (10 000 entries, removed when a lookup no longer knows the hash), so a flood of unknown tokens cannot crowd out working senders.
- **Bounded memory.** Per-token limiter at most 100 000 keys, per-IP limiter at most 10 000 (both `auth.Limiter`, which evicts expired then arbitrary entries), LRU 10 000.
- **Metrics.** `maiak_heartbeat_pings_total{result}`: `accepted`, `ignored`, `unknown` (404, malformed included), `rate_limited` (per-token, or per-IP for unknown tokens), `busy` (new: in-flight bound), `error`.
- **Config check.** Migration `20261021180000_heartbeat_hardening` requires `config.graceSeconds` to be a JSON number whose text matches `^[0-9]+$`, still 0-3600. The expected interval lives in the integer column `interval_seconds` and needs no pattern. Existing integral spellings (`60.0`) are rewritten to the integer per tenant (the owner is subject to forced RLS); any other invalid row fails the migration with the monitor ids. `list_due_heartbeats` and the dashboard query cast through `::numeric::integer` as defence in depth.
- **Ping time.** `heartbeat_ping` (same signature, `CREATE OR REPLACE`) first locks the token row, then sets `last_ping_at = greatest(last_ping_at, clock_timestamp())`: a ping that waited for an evaluation records a time after it, and the value never moves backwards.
- **Pings while up.** The evaluation applies an OK result through `incidents.Service.ApplyResult` only when the heartbeat is not `up`; for an `up` heartbeat it only advances `applied_ping_at` under the row lock. Recovery from `down` or `unknown` is unchanged. `monitor_state.last_checked_at` of a heartbeat is therefore the time of the last state-changing result; the UI shows `lastPingAt`.

## Consequences
- A known token is limited only by its own one-per-second limit and, under overload, by the in-flight bound.
- Unknown tokens still cost one lookup each until their IP is over the limit, and after it as well (the lookup decides between 204 and 429); the in-flight bound caps the concurrent cost.
- A steady heartbeat costs one `UPDATE heartbeat_tokens` per evaluation instead of the incident machine's writes (L-202 updated).
- Tests: malformed tokens never reach the handler's database path and are not read; a known token from an exhausted IP and from the exhausted shared bucket gets 204; the in-flight bound and the LRU bound; the check rejects `60.0`; the migration rewrites `60.0` and fails on invalid rows; ping time after a lock wait and never backwards; both evaluation paths.

## Alternatives considered
- Per-IP reservation before the lookup with refund on success (ADR-0094): lets unknown traffic block valid pings (the review finding).
- Per-IP check before the lookup only for hashes not in the LRU: see above; a cold cache turns into a permanent block.
- A semaphore that blocks instead of answering 429: holds goroutines and connections under a flood; Alertmanager retries a 429.
