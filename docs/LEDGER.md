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
| L-024 | deferred | open | Client-up-to-date CI check for the web | Needs the generated client from F-02 | With F-02 |
| R-010 | risk | open | Backend and web bump jobs both push to `main` | `release.yml` runs them in parallel; the retry loop with `git reset --hard` is untested | First CI run of `release.yml` |
| R-009 | risk | open | RDS admin login is reachable by the bootstrap Job | While `dbBootstrap.enabled` is true, the Job holds the `grafana` RDS master login (Grafana's own). Anyone able to change this chart or its image (built from a personal GitHub repository) can use it; on PostgreSQL 15 there is no least-privilege alternative. Disable after the first success; use a dedicated RDS (L-009) | At the first successful deploy (switch the flag off), and whenever the flag is enabled again |
| R-008 | risk | open | Release workflow pushes the image-tag bump straight to `main` | `release.yml` commits `values-platform.yaml` as github-actions[bot]. Branch protection on `main` would block it; it needs a bypass for the bot or a PR-based flow | When enabling branch protection |

## Done

| ID | Title | Closed by |
|---|---|---|
| L-015 | Frontend stack | [ADR-0015](adr/0015-frontend-stack.md): minimal React + Vite, Pico.css, generated client (2026-10-20) |
| L-021 | First ArgoCD sync: PreSync migration hook ran before the ExternalSecret existed, and in Sync mode the Job (wave 1) would start after workloads (wave 0) | Chart: ExternalSecret and migrate ServiceAccount wave -2, migrate Job `Sync` hook wave -1, workloads wave 0; `migration.hookPhase` defaults to `Sync` (2026-10-20) |
