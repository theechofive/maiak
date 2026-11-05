# ADR-0101: Phase 3: notification channels, in-cluster agent, watchdog verification

- Status: Accepted
- Date: 2026-10-27

## Context
Client 2 needs internal components monitored from inside its cluster, an Alertmanager watchdog (already built in P-03) and configurable Slack notifications. The public domain comes later, so everything is built and verified on the internal network first (`klr-platform` is the first test cluster).

## Decision
- **Notification channels (S-01):** a tenant has several Slack webhook channels (limited by the plan: `free` 1, `internal` 10). Each channel has a name, an encrypted webhook URL (write-only), the events it receives (`down`, `recovered`, `tls_expiring`, later `agent_offline`, `agent_online`), an optional mention (`@here`, `@channel`, a user-group or user reference, validated against a strict pattern and used only in `down` and `reminder` messages), and an optional reminder interval for incidents that stay open (off by default). Channels can be replaced, deleted and tested. A monitor chooses its channels (`channelIds`); `notifySlack` with no selection means all active channels of the tenant. Supersedes ADR-0019 (one channel per tenant, `monitor_channels` unused). Slack OAuth stays out of scope.
- **Agent (A-series):** a `cmd/agent` binary in the same image, deployed to the client's cluster by a Helm chart `deploy/helm/maiak-agent`. It only makes outbound HTTPS calls to Maiak. Each agent has its own token (32 random bytes, only the hash stored, shown once, revocable). First checks: HTTP(S) and TCP inside the client's network; private addresses are allowed for agents (the SSRF guard protects the hosted worker, not the client's own network). Kubernetes resource checks come in a later wave.
- **Agent gateway:** a separate process `agentgw`, running with the worker's database role, serves the agent API (`claim`, `results`, heartbeat of the agent itself). The `api` role keeps having no write access to scheduler and state tables (ADR-0040) and the surface exposed to clients is isolated from the dashboard. Authentication goes through a definer function `agent_authenticate(token_hash)`.
- **Locations:** every agent owns a `probe_locations` row of kind `agent`, bound to its tenant; a monitor picks "Maiak cloud" or one agent through `monitor_locations`. Quota `max_agents` (`free` 1, `internal` 10).
- **Agent offline:** an agent silent for longer than its threshold makes its monitors `unknown` (not `down`, to avoid false alarms) and raises an `agent offline` incident with a Slack notification.
- **Watchdog:** no new feature; it is verified live against the VMAlertmanager of `klr-platform` with the owner's hands on the Alertmanager config. The monitor page gets the last ping and a short ping history for debugging.
- **Order:** S-01 channels, A-01 agent protocol and gateway, A-02 agent UI and monitor location, A-03 charts, A-04 agent offline, V-01 live verification, K-01 Kubernetes checks.

## Consequences
- More processes in the cluster (`agentgw`), small requests as before.
- Agents work only where Maiak is reachable; until the public domain, that is the internal network.
- Distribution of the agent chart and image to external clients needs public packages or pull secrets (ledger).
