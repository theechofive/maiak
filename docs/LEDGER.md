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
| L-001 | deferred | open | SSRF details | Baseline guard is mandatory in the MVP (ADR-0011). Still undecided: allowed ports (proposal 443 and 8443), whether blocked-URL errors show the reason | Before the first non-admin tenant |
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
| L-015 | deferred | open | Frontend stack | Owner decides; the API contract and TS client generation work with any framework. Web image build job is a disabled placeholder in CI | Before slice 5 (dashboard) |
| L-016 | deferred | open | Metadata and docs language | Docs and code are written in English by assumption (not discussed). Reverse the decision if the team wants Ukrainian | Before the first external contributor |
| L-017 | deferred | open | Network egress cost control | Default is to not read response bodies (cap 64 KB only with keyword checks). NAT cost at scale needs a re-estimate | During the load test (L-010) |
| L-018 | deferred | open | RDS TLS verification | DB URLs use `sslmode=require` (encrypted, server not verified). Move to `verify-full` with the RDS CA bundle in the image | Before production clients |
| R-001 | risk | open | Shared RDS `grafana` | One `db.t4g.small`, one AZ, shared with Grafana. A failure or maintenance window hits both, and Maiak is supposed to alert about outages | Same as L-009 |
| R-002 | risk | open | NetworkPolicy is not enforced | VPC CNI network-policy agent is disabled in `klr-dev` and `klr-platform`. SSRF defence rests only on application code. Chart ships policies disabled | Permanent; revisit if the cluster enables enforcement |
| R-003 | risk | open | `klr-platform` memory is tight | Memory requests are at 71-81% of allocatable. Chart requests are intentionally small | Whenever replicas or limits grow |
| R-004 | risk | open | Single-AZ nodes in both clusters | An AZ outage takes the platform down | After MVP |
| R-005 | risk | open | No public ingress in `klr-platform` | Only the internal Envoy Gateway exists. Remote probes and client agents will need a public entry for one path prefix | Before L-005 / L-006 |
| R-006 | risk | open | Default kubectl context is a production cluster | The developer's current context is `klr-prod`. Always pass an explicit `--context` | Permanent |
| R-007 | risk | open | Worker with `hostNetwork` would reach IMDS | Node IMDS is v2 with hop limit 1, which protects only non-hostNetwork pods. The chart must never set `hostNetwork` on the worker | Permanent |

## Done

| ID | Title | Closed by |
|---|---|---|
| | | |
