# ADR-0092: Self-service tenants, plans with quotas, heartbeat monitors, platform admin

- Status: Accepted
- Date: 2026-10-21

## Context
Two external companies are potential clients. The second one needs domain checks, internal-service checks, an Alertmanager watchdog and a status page. The owner wants anyone to be able to register on a free plan (3 monitors with default values and 1 watchdog), plans with quotas, and a minimal admin panel. Public exposure comes later, on a decision of the owner. The first client (geo checks, YAML list watcher) is postponed.

## Decision
- **Order:** (1) plans and quotas, (2) registration, (3) heartbeat monitors (watchdog), (4) platform admin panel, then (5) status page and in-cluster agent. Task list: `docs/tasks/phase2.md`.
- **Plans:** a global `plans` table (key, name, limits jsonb) and `tenants.plan_key`. Seeded plans: `free` (3 monitors, 1 heartbeat, 1 notification channel) and `internal` (generous limits, used by the owner's `default` tenant). Per-tenant overrides stay in `tenants.limits`. Limits are enforced on the server. Paid plans exist only as data, there is no billing.
- **Registration:** email and password, no email verification and no outgoing email for now. A registration creates a tenant on the `free` plan and its first user as `owner`, and logs the user in. A kill switch `MAIAK_REGISTRATION_ENABLED` exists. Password recovery is performed by the platform admin until email delivery exists. The system login `admin` coexists with email logins.
- **Heartbeat (watchdog) monitors:** a monitor type `heartbeat` with an unguessable token URL. The token is stored only as a hash and shown once (it can be regenerated). A ping needs no session. A missed interval plus grace opens an incident through the existing incident machine; a ping resolves it. Heartbeats count against their own quota.
- **Platform admin:** `users.is_platform_admin`, an admin API and UI for tenants (list, plan change, suspend, delete) and users (password reset). Cross-tenant access goes through `SECURITY DEFINER` functions that check the flag inside, never through a role that bypasses RLS. Every action is audited. No impersonation.

## Deferred (ledger)
Email verification, outgoing email and password reset by email (L-170); Turnstile and signup rate limits (L-171); domain ownership verification (L-011); launch conditions for the public domain, to be revisited when the owner decides to move to separate infrastructure (L-172).

## Consequences
- Open registration without verification is acceptable only while the service is reachable from the VPN; the risk is recorded (R-130) and the kill switch is the mitigation.
- The user model changes: real emails next to the system login (closes L-160 and R-120 in task P-02).
- A public heartbeat endpoint is the first request path without a session; it must be cheap, constant-time and rate limited.
