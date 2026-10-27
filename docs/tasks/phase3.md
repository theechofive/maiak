# Phase 3: notification channels, agent, watchdog verification

Decisions: [ADR-0099](../adr/0099-phase3-agent-channels.md). Conventions as in [README.md](README.md): one commit per task, independent review after each task, ledger entries for shortcuts. Public domain is later: everything is built and verified on the internal network, first in `klr-platform`.

| ID | Task | Depends on | Status |
|---|---|---|---|
| S-01 | **Notification channels.** Several Slack channels per tenant (plan `max_channels`: free 1, internal 10); API `/api/v1/channels` (list, create, get, patch, delete, test) replacing `/settings/slack`; fields name, write-only webhook, events (`down`, `recovered`, `tls_expiring`), optional validated mention, optional reminder interval (5 to 1440 minutes, off by default); monitors choose `channelIds` (empty with `notifySlack` means all active channels); outbox creates one delivery per matching channel and respects event filters; `reminder` deliveries for incidents that stay open; deleting a channel fails its pending deliveries; web: channels list and form in Settings, channel selection on the monitor form; closes L-081. Done: [ADR-0100](../adr/0100-notification-channels.md). | none | done |
| A-01 | **Agent protocol and gateway.** Tables for agents (tenant, name, token hash, last seen, version, revoked), an agent `probe_locations` row per agent, `max_agents` in plans, TCP monitor type, `agent_authenticate` definer function, process `cmd/agentgw` (claim, submit results, agent heartbeat) on the worker role, `cmd/agent` executor (HTTP(S) without the SSRF guard, TCP, retries, graceful shutdown, config by env), OpenAPI for the agent API (separate spec section), tests incl. a full agent-to-incident flow. | S-01 | todo |
| A-02 | **Agent UI and monitor location.** Agents page (create with one-time token and a ready `helm install` command, online/offline, version, last seen, revoke), monitor form: "Run from" (Maiak cloud or an agent) and the TCP type, location shown in lists and details. | A-01 | todo |
| A-03 | **Charts and release.** `deploy/helm/maiak-agent` chart, `agentgw` Deployment and route in the Maiak chart (internal only for now), release workflow publishes the agent chart, validation scripts. | A-01 | todo |
| A-04 | **Agent offline.** Offline threshold, monitors of an offline agent become `unknown`, `agent offline` and `agent online` incidents with Slack events, metrics. | A-01, S-01 | todo |
| V-01 | **Live verification in klr-platform.** Owner adds the watchdog receiver to the VMAlertmanager config (prepared snippet and runbook), an agent runs in `klr-platform` against internal services; checks of pings, incidents and Slack. Last ping and a short ping history on the heartbeat monitor page. | A-02, A-03 | todo |
| K-01 | **Kubernetes checks in the agent** (Deployment ready replicas, Pod status) with read-only RBAC. | A-04 | design |

## Deferred (see the ledger)
Slack OAuth app, Telegram and email channels (L-014), public domain launch conditions (L-172), distribution of agent chart and image to external clients (packages must be public or pull secrets provided).
