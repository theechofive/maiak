# Ledger

Single register of deferred work, problems and risks. Protocol is in `CLAUDE.md`.

- **IDs are stable and never reused.** `L-nnn` = deferred work or problem, `R-nnn` = risk.
- **Status:** `open`, `blocking` (blocks the next milestone), `done`, `wontfix`.
- **Revisit trigger:** the event that must make us look at the entry again. Entries with no trigger rot.
- Code references: `TODO(L-nnn)`.

Created 2026-10-20 from the design session.

## Open entries

| ID | Type | Status | Title | Context | Revisit trigger |
|---|---|---|---|---|---|
| L-001 | deferred | open | SSRF details | Baseline guard is mandatory in the MVP (ADR-0011). Decided for the MVP: port 443 only (ADR-0016). Still undecided: whether blocked-URL errors show the reason, wider port policy | Before the first non-admin tenant |
| L-002 | deferred | blocking | ghcr pull secret in `klr-platform` | No pull-secret convention exists in the cluster. Options: ExternalSecret `platform/maiak/ghcr` (dockerconfigjson with a `read:packages` PAT), ECR in the platform AWS account, or public images | Before the first deploy (slice 1) |
| L-003 | deferred | open | Self-monitoring of Maiak | `ServiceMonitor` plus a VMAlert rule (checks counter not growing for 5 min) via the existing VictoriaMetrics/Alertmanager stack. The `/metrics` endpoint exists from slice 1 | Before relying on Maiak alerts operationally |
| L-004 | deferred | open | Public status page | Per-tenant page on a subdomain, cached snapshots behind a CDN, separate from the API. Incident data model is already prepared | After MVP |
| L-005 | deferred | open | In-cluster agent for internal services | `cmd/agent` shares the probe core and the pull/push protocol (ADR-0006). Needs a public entry for `/probe` (see R-005) | After MVP |
| L-006 | deferred | open | Regional probes: regions, providers, provisioning | `cmd/probe` without DB access. Pick first regions/providers, Terraform plus container on small VMs (not a K8s cluster per region), public entry for `/probe` | After the agent |
| L-007 | deferred | open | Multiple users, OAuth, roles | Schema has `users`, `memberships`, `auth_identities` already. Needs invitations, UI, OAuth providers, per-replica-safe rate limiting | Before onboarding any client |
| L-008 | deferred | open | Billing, plans, quotas | `tenants.limits` jsonb exists; plans and payment provider are not designed | Before commercial launch |
| L-009 | deferred | open | Dedicated Multi-AZ RDS instead of the shared `grafana` instance | See R-001. Move is a dump/restore or logical replication, no code change | Before production clients or when Maiak alerts become critical |
| L-010 | deferred | open | Load test with 100 000 synthetic monitors | Validate the scale analysis (ADR-0012): scheduler claim rate, insert rate, autovacuum on state tables | Before open registration |
| L-011 | deferred | open | Domain ownership verification and abuse limits | Otherwise a tenant can aim checks at third-party sites from our IPs (DNS TXT verification, per-tenant rate limits) | Before open registration |
| L-012 | deferred | open | Protection against mass false-positive alerts | If our own egress fails, all monitors go down at once: canary targets, confirmation from a second location, global kill switch | When the second location appears |
| L-013 | deferred | open | ClickHouse for results, sharded scheduler, cells | Scale stage 2/3 in ADR-0012. The company already runs ClickHouse in `klr-dev` | At stage 2 (about 1 000 tenants or 800+ checks/s) |
| L-014 | deferred | open | SSE, Slack OAuth app, TCP/DNS/k8s checks | Out of MVP scope; each is additive | On demand |
| L-016 | deferred | open | Metadata and docs language | Docs and code are written in English by assumption (not discussed). Reverse the decision if the team wants Ukrainian | Before the first external contributor |
| L-017 | deferred | open | Network egress cost control | Default is to not read response bodies (cap 64 KB only with keyword checks). NAT cost at scale needs a re-estimate | During the load test (L-010) |
| L-018 | deferred | open | RDS TLS verification | DB URLs use `sslmode=require` (encrypted, server not verified). Move to `verify-full` with the RDS CA bundle in the image | Before production clients |
| L-019 | deferred | open | Pin CI actions and tools | GitHub Actions use major tags with `# TODO pin to SHA`; `oasdiff`, `golangci-lint` and spectral versions are unpinned. Dependabot is configured | Before the first CI run on `main` or before the repo becomes public |
| L-020 | deferred | open | Database role of `jobs` and partition DDL | Chart gives `jobs` the `worker_url` (least privilege). Creating partitions needs owner privileges, decided in ADR-0016: `SECURITY DEFINER` functions owned by `maiak_owner` (search_path pinned) for partitions, rollups, outbox claiming and session purge, rather than giving `jobs` the owner role; keep this entry open until they are implemented and tested | Slice 3 (results and partitions) |
| R-001 | risk | open | Shared RDS `grafana` | One `db.t4g.small`, one AZ, shared with Grafana. A failure or maintenance window hits both, and Maiak is supposed to alert about outages | Same as L-009 |
| R-002 | risk | open | NetworkPolicy is not enforced | VPC CNI network-policy agent is disabled in `klr-dev` and `klr-platform`. SSRF defence rests only on application code. Chart ships policies disabled | Permanent; revisit if the cluster enables enforcement |
| R-003 | risk | open | `klr-platform` memory is tight | Memory requests are at 71-81% of allocatable. Chart requests are intentionally small | Whenever replicas or limits grow |
| R-004 | risk | open | Single-AZ nodes in both clusters | An AZ outage takes the platform down | After MVP |
| R-005 | risk | open | No public ingress in `klr-platform` | Only the internal Envoy Gateway exists. Remote probes and client agents will need a public entry for one path prefix | Before L-005 / L-006 |
| R-006 | risk | open | Default kubectl context is a production cluster | The developer's current context is `klr-prod`. Always pass an explicit `--context` | Permanent |
| R-007 | risk | open | Worker with `hostNetwork` would reach IMDS | Node IMDS is v2 with hop limit 1, which protects only non-hostNetwork pods. The chart must never set `hostNetwork` on the worker | Permanent |
| L-022 | deferred | open | Verify bootstrap hook behaviour at the first real sync | Unverified assumptions: ArgoCD deletes the `maiak-db-admin` ExternalSecret and its Secret after the Job succeeds (`HookSucceeded`); the bootstrap RBAC is sufficient; External Secrets resolves `platform/grafana/db`. Consider a PostSync check that `maiak-db` exists with all keys | At the first deploy (slice 1) |
| L-023 | deferred | open | Web image never built or run | Docker was not running when F-10 was written: the Dockerfile, nginx config (read-only rootfs, arbitrary uid) and probes (chart still probes `/`, nginx serves `/healthz`) are untested | Before the first deploy; first CI run of `release.yml` |
| R-010 | risk | open | Backend and web bump jobs both push to `main` | `release.yml` runs them in parallel; the retry loop with `git reset --hard` is untested | First CI run of `release.yml` |
| L-025 | deferred | open | Expired-session cleanup | `purge_expired_sessions()` exists since B-10 (tested); `jobs` must call it (B-14); until then expired rows accumulate (harmless, lookups check `expires_at`) | B-14 |
| L-026 | deferred | open | Digest-pinned base images | `golang` and distroless images are pinned by tag only; part of L-019 | With L-019 |
| R-011 | risk | open | Local DB tests ran on PostgreSQL 16; CI and RDS use 15 | The throwaway test database was version 16; first CI run is the real check on 15 | First CI run |
| R-012 | risk | open | Old pod with the previous admin email rotates the admin back | During a rollout after changing `MAIAK_ADMIN_EMAIL`, a restarting old pod re-applies its own email at bootstrap | When rotating the admin email: scale to zero first |
| R-013 | risk | open | Requests without X-Forwarded-For share one limiter bucket | They can lock each other out of login | If the gateway ever stops adding XFF |
| R-009 | risk | open | RDS admin login is reachable by the bootstrap Job | While `dbBootstrap.enabled` is true, the Job holds the `grafana` RDS master login (Grafana's own). Anyone able to change this chart or its image (built from a personal GitHub repository) can use it; on PostgreSQL 15 there is no least-privilege alternative. Disable after the first success; use a dedicated RDS (L-009) | At the first successful deploy (switch the flag off), and whenever the flag is enabled again |
| R-008 | risk | open | Release workflow pushes the image-tag bump straight to `main` | `release.yml` commits `values-platform.yaml` as github-actions[bot]. Branch protection on `main` would block it; it needs a bypass for the bot or a PR-based flow | When enabling branch protection |
| L-030 | deferred | open | Wiring and cross-package test route for the SSRF guard | `safehttp`'s loopback dial override is an unexported option set only from its own `_test.go` files (enforced by `TestOverrideNotSetOutsideTests`), so `checks` tests use a plain routing client for the prober's own behaviour. B-16 needs a cross-package route (e.g. a `//go:build maiaktest` file exporting a setter, never in production builds). The worker must pass `config.Worker.BlockedCIDRs` (`MAIAK_BLOCKED_CIDRS`) to `safehttp.Options.BlockedPrefixes` with `MaxRedirects: 3`; Slack (B-13) uses `MaxRedirects: 0` | B-12, B-13, B-16 |
| R-020 | risk | open | SSRF guard blocks some legitimate targets | Whole NAT64 (`64:ff9b::/96`), 6to4, Teredo/`2001::/23` and `::/96` ranges are blocked instead of decoding the embedded IPv4, and a hostname is refused when ANY resolved address is blocked (split-horizon DNS or a stray private record fails the check as `blocked_address`) | First customer report of an unexpected `blocked_address` |
| L-028 | deferred | open | Mixed field-name casing in the API | Auth resources (`csrf_token`, `expires_at`, `display_name`) and `request_id` in problems are snake_case; everything added in B-05 is camelCase (ADR-0017). Renaming is breaking | Before the API is offered to customers (API keys) or at the first `/api/v2` |
| R-014 | risk | open | `oasdiff` reports B-05 as breaking | `logout` gained the required `X-CSRF-Token` header parameter (behaviour unchanged: the security requirement already needed it). The CI breaking-change job compares with the base branch and will flag it once | First CI run after B-05; accept the finding or add an oasdiff ignore entry |
| L-027 | deferred | open | Pause and delete do not resolve open incidents yet | ADR-0016 says pause and delete resolve an open incident (reason `paused`/`deleted`, no notification). The incidents table and service arrive in B-11; hooks are marked `TODO(L-027)` in `internal/monitors/service.go` | B-11 |
| L-040 | deferred | open | Partition maintenance limits | `drop_expired_partitions()` proves rollup completeness by scanning the whole expired partition (one GROUP BY per month); there is no default partition, so if `jobs` does not call `ensure_partitions()` for about two months, result inserts fail (ADR-0018). Alert on the partition horizon with L-003 | Load test (L-010) or the first failed result insert |
| L-050 | deferred | open | `openapi-typescript` peer dependency forced to TypeScript 6 | `openapi-typescript@7` declares `typescript@^5`; `web/package.json` uses an npm `overrides` entry so `npm ci` works with TypeScript 6. Generation works and CI checks its output | When `openapi-typescript` supports TypeScript 6, or when `gen:api` output changes unexpectedly |
| L-051 | deferred | open | Delete confirmation is a non-modal dialog | Monitor details render `<dialog open>` (Pico styles it as an overlay) with focus on Cancel and Escape to close, but no focus trap or inert background; jsdom has no `showModal` for tests | Accessibility review, or when a second confirmation dialog is needed |
| L-052 | deferred | open | Web UI only tested in jsdom | F-02..F-09 were verified with Vitest and mocked fetch; the SPA was never run in a browser against the real api (several endpoints still answer 501). Unverified: cookie and CSRF flow end to end, Pico layout, chart rendering, dev proxy against a remote API | First deploy with B-10..B-16 done |
| L-053 | deferred | open | CSP `style-src 'unsafe-inline'` probably unnecessary | The SPA uses no inline `style` attributes (chart styled by CSS classes); `web/security-headers.conf` still allows `'unsafe-inline'` for styles | With L-052: check the built app in a browser without it, then remove |
| L-054 | deferred | open | Details status line takes the HTTP code from the latest check | `MonitorDetail` has `lastErrorKind` but no last HTTP status, so "Down for 12 min · HTTP 503" reads `httpStatus` of the newest item of `GET /checks` and falls back to the error kind when checks are unavailable | If the status line is wrong in practice, add `lastHttpStatus` to the contract |

## Done

| ID | Title | Closed by |
|---|---|---|
| L-015 | Frontend stack | [ADR-0015](adr/0015-frontend-stack.md): minimal React + Vite, Pico.css, generated client (2026-10-20) |
| L-021 | First ArgoCD sync: PreSync migration hook ran before the ExternalSecret existed, and in Sync mode the Job (wave 1) would start after workloads (wave 0) | Chart: ExternalSecret and migrate ServiceAccount wave -2, migrate Job `Sync` hook wave -1, workloads wave 0; `migration.hookPhase` defaults to `Sync` (2026-10-20) |
| L-024 | Client-up-to-date CI check for the web | F-02: the `web` CI job runs `npm run gen:api` and fails when `web/src/api/generated` changes; the job also runs on `api/**` changes (2026-10-20) |
