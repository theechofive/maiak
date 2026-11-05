# ADR-0102: Organizations, roles, invitations and projects

- Status: Accepted
- Date: 2026-11-05

## Context
A tenant has exactly one user today: registration creates a tenant with its owner (P-02), the system admin owns `default`, and there is no way to add a second person (L-007). `memberships` already has a `role` in `owner`/`admin`/`member`/`viewer`, but nothing reads it. A login binds to one membership and cannot switch (L-190). Client 2 wants several people in one account and its monitors split by team or product; the status page (ADR-0103) needs a role allowed to publish. Phase 4 plans this as T-01 (members and roles) and T-02 (projects), [phase4.md](../tasks/phase4.md).

## Decision
### Organizations and roles (T-01)
- **Tenant = organization.** No new table; the UI says "organization". RLS stays per tenant (ADR-0004); roles and projects are enforced by the application, not by RLS (R-212).
- **Roles** (`memberships.role`, unchanged schema):

  | Role | May |
  |---|---|
  | `owner` | everything an admin may, plus delete the organization (soft delete as in ADR-0097) and change the plan once billing exists (today only the platform admin changes plans, L-008); grant or revoke `owner` |
  | `admin` | members and invitations (not owners), notification channels, projects and their members, the status page, every monitor |
  | `member` | read its projects; create, edit, pause and delete monitors and heartbeats in projects it belongs to |
  | `viewer` | read its projects only |

  An organization always keeps at least one active owner (last-owner demotion, removal and leaving are refused). Ownership transfer is "grant owner, then step down".
- **One place for authorization.** A pure package `internal/authz` holds the action-to-role table and answers `Can(actor, action, project)`. The api resolves the actor (user, organization role, visible projects) once per request from the session's membership, so a role change applies on the next request, and a middleware checks the operation's minimum role by its OpenAPI operation id; project-scoped checks and list filters call the same package inside the services. An operation missing from the table is denied, and a test fails when the spec has an operation the table does not list. Denials are 403 `problem+json` (`forbidden`).
- **Several organizations per user.** `GET /api/v1/auth/organizations` lists the user's memberships through a definer function (`auth_user_organizations(user_id)`, cross-tenant read limited to that user). `POST /api/v1/auth/switch-organization` checks the membership and the tenant status, then replaces the session with a new one bound to the target tenant (new token and CSRF token). The web shows a switcher in the layout. Login keeps `auth_login_tenant`. Closes L-190.
- **Invitations without email.** An admin creates an invitation (role `admin`, `member` or `viewer`, optional label for the admin's own reference, expiry 7 days); the response shows a link with a single-use token once (32 random bytes, URL-safe, only the SHA-256 hash stored, like heartbeat tokens). The admin hands the link over; no email is sent (L-170, L-273). Invitations can be listed and revoked. The link page looks the token up through a definer function before any tenant is known (constant answer for unknown, expired, used and revoked tokens):
  - a logged-in user accepts it and becomes a member, and the session switches to the organization;
  - a new person registers through the link (`POST /auth/register` with `inviteToken`): user and membership are created, no tenant of their own. Allowed also when `MAIAK_REGISTRATION_ENABLED` is false, because the invitation is the authorization.
  Acceptance takes the tenant row lock, marks the invitation used and inserts the membership in one transaction. A user who is already a member gets 409.
- **Plan limit** `max_members` (initial values `free` 3, `internal` 50), counting memberships plus open invitations, checked with `plans.CheckCreate` under the tenant row lock (ADR-0093).
- **Member management.** List members (email, role, joined), change a role, remove a member (deletes that user's sessions in this tenant). Admins cannot touch owners.
- **Audit:** `member.invited`, `invitation.revoked`, `member.joined`, `member.role_changed`, `member.removed`, `organization.switched` (no tokens in metadata).

### Projects (T-02)
- **Tables** (module `projects`, forced RLS, `tenant_isolation` policy, composite `(tenant_id, id)` keys, schema lint):
  - `projects`: `tenant_id`, `id`, `name` (1-60), `slug` (unique per tenant), `is_default` (one per tenant, partial unique index), `all_members` (true for the Default project), timestamps, `deleted_at`.
  - `project_members`: PK `(tenant_id, project_id, user_id)`, composite FKs to `projects` and `memberships`, optional `role` override (`member` or `viewer`; NULL = the organization role).
- **Default project.** Every tenant gets a "Default" project: the migration backfills one for each existing tenant, and tenant creation (`auth_create_tenant`) creates it. `all_members` makes every member of the organization a member of it without rows. It cannot be deleted. Other projects can be deleted only when they have no live monitors or channels.
- **Ownership of data.** `project_id` on `monitors` (heartbeats included), `notification_channels` (NULL = organization-wide channel, admin only) and later agents (A-01 after this task). Composite FK `(tenant_id, project_id)`.
- **Visibility.** Owners and admins see every project. Members and viewers see the projects they belong to (the Default project included) and only their monitors, incidents, results and channels; `internal/queries` filters by the actor's project ids. The heartbeat ping endpoint is unchanged.
- **Channel routing.** A monitor selects channels of its own project plus organization-wide ones. With `notifySlack` and an empty selection, a monitor notifies the active channels of its project only (not organization-wide ones). Moving a monitor to another project drops its selected channels of the old project; moving a channel drops links from monitors of other projects. This narrows the fallback of L-261 to one project and makes the rule explicit (L-263); both are closed by T-02 by the owner's decision.
- **Quotas stay per organization** (monitors, heartbeats, channels, members). Projects have a fixed cap of 50 per organization, not a plan limit.
- **Migrations, expand/contract.** (1) add `projects`, `project_members`, nullable `project_id` columns, backfill the Default project and point every existing monitor and channel at it (existing channels go to the Default project, so the empty selection of existing monitors keeps its meaning); the previous app ignores the columns. (2) After the release: backfill monitors the previous api created during the rollout, then `monitors.project_id NOT NULL`. Channels stay nullable by design, so a channel the previous api creates during the rollout becomes organization-wide; accepted (short rollout, the admin can move it), recorded in the ledger by T-02 if it happens.
- **API.** `/api/v1/projects` (list, create), `/projects/{projectId}` (get, patch, delete), `/projects/{projectId}/members` (list, put, delete); `projectId` on monitor and channel resources (optional on create, default the Default project); `?projectId=` filter on the monitor list. Additive, no breaking change.
- **Web.** Project switcher and filter in the layout ("All projects" for owners and admins), a Projects page (create, rename, delete, members), a project field on the monitor and channel forms, the channel picker limited to the monitor's project plus organization-wide channels.

## Consequences
- The database isolates organizations; isolation between projects of one organization depends on `internal/authz` and the query filters (R-212). Every handler test gets a role matrix case.
- Invitation links are bearer secrets until used or expired (R-213).
- The quota is shared by all projects; one project can use the whole organization's limit.
- SSO, API tokens, groups and per-user notification preferences are not part of this (L-269). Sharing one channel with a subset of projects is not possible: a channel belongs to one project or to the whole organization (L-271).
- Phase 3 agent tasks wait; agents are created per project after T-02.

## Implementation notes (T-01, 2026-11-05)
Choices made while building T-01 within this decision:
- **Operation table.** `internal/authz` maps every operation id to a minimum role: reads `viewer`; monitor and heartbeat changes `member`; channels (including the test message), members and invitations `admin`; `deleteOrganization` `owner`; the platform admin operations keep their own flag check. A strict-handler middleware applies it after the request validator; `TestAuthzTableCoversSpec` and `TestIntegrationRoleMatrix` (expected roles written down independently of the table) guard it. The role is read from `memberships` on every request (L-279).
- **Member list** is readable by every role; invitations are admin-only (L-278). An admin changes and removes admins, members and viewers, never owners, and never grants a role above its own; owners are granted only by a role change, never by an invitation. All member changes, invitation creates and acceptances take the tenant row lock, so two owners cannot demote each other at once.
- **Extra operations** the decision implies: `POST /organization/leave` (any role; refused for the last owner and the platform admin), `DELETE /organization` (owner; soft delete through the definer `org_delete_tenant`, which acts only on the transaction's tenant and refuses `default`), `POST /invitations/lookup` (public, for the invitation page), and revoke as `POST /invitations/{id}/revoke`.
- **Bearer secret (R-213).** The link is `<MAIAK_PUBLIC_URL>/invite#<token>`: the fragment is never sent to a server, and the web sends the token only in request bodies. Failed token attempts are limited per client IP (L-276).
- **Platform admin.** It cannot accept invitations (409), because `BootstrapAdmin` refuses to start when the admin belongs to another tenant (L-277); its membership of `default` cannot be changed or removed. `BootstrapAdmin` now retires only previous owners that still carry the platform-admin flag (an earlier configured admin), never owners granted through member management.
- **Quota.** An open invitation holds its place in `max_members`; accepting re-checks only that the tenant is not above its limit (a lowered plan), with `plans.CheckJoin`.
- **Sessions.** Switching and accepting create the new session, then delete the old one (L-280). Removing a member deletes its sessions in that tenant through the existing `ON DELETE CASCADE`.

## Alternatives considered
- Project-aware RLS (`app.project_ids` set per transaction): stronger, but every definer function, rollup and outbox path would need it, and a member's project list changes at runtime. Kept as the upgrade path if R-212 bites.
- Invitations bound to an email address: needs outgoing email to mean anything (L-170). The link is single-use, expires, and the admin sees who joined.
- Projects as tags on monitors: no membership, no channel ownership, no place for agents.
- Empty selection = project channels plus organization-wide channels marked as default: one more flag and a rule users must learn; kept simple.
