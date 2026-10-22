# Runbook: platform admin panel

The panel is the `/admin` area of the web app and the `/api/v1/admin/*` operations behind it ([ADR-0097](../adr/0097-platform-admin-panel.md)). Only the platform admin can use it: the system admin configured by `MAIAK_ADMIN_LOGIN` (default `admin`, credentials in the `maiak-admin` Secret). Every other user gets 403 from the API and the not-found page in the web app.

## Who is the platform admin
- `users.is_platform_admin` is set by the api at every start for the configured login and cleared for every other user. There is no API or UI to grant it.
- Changing `MAIAK_ADMIN_LOGIN` moves the flag to the new login at the next rollout; the previous admin is disabled (ADR-0009) and loses the flag.
- To check: `SELECT email, status FROM users WHERE is_platform_admin;` as `maiak_owner` (one row expected).

## What the panel does
| Action | Where | Effect | Audit row (in the target tenant) |
|---|---|---|---|
| Overview | `/admin` | Counts: tenants (active, suspended, deleted), active users, monitors, heartbeats, tenants per plan | none |
| List and search tenants | `/admin/tenants` | Search by name, slug, tenant id or the email of a member; 25 per page | none |
| Change plan | tenant details, plan selector | The tenant's limits change at once (see "Plans and existing usage") | `admin.tenant.plan_changed` with `from`, `to` |
| Suspend / unsuspend | tenant details | See "Suspension" | `admin.tenant.suspended` / `admin.tenant.unsuspended` |
| Delete | tenant details | Soft delete, see "Deletion" | `admin.tenant.deleted` |
| Reset a password | tenant details, users table | New temporary password, all sessions of the user deleted | `admin.user.password_reset` with the number of sessions deleted |

Guard rails (enforced in the database, not only in the UI): the `default` tenant cannot be suspended or deleted, the admin cannot suspend or delete a tenant it is a member of, a deleted tenant cannot be changed, and the system admin's password cannot be reset in the panel. `maiak_admin_actions_total{action}` counts the changes ([observability](observability.md)).

## Recover a customer's password
Until outgoing email exists (L-170) this is the only recovery path.
1. Verify the request through a channel you trust (the customer's known contact, not only an email that claims to be them). Without email verification, registration proves nothing about who owns an address (R-120).
2. Open `/admin/tenants`, search for the customer's email, open the tenant.
3. In the users table choose **Reset password** for that user and confirm.
4. Copy the temporary password from the panel. It is shown once: the server keeps only its hash, and hiding the panel or leaving the page clears it from the page (the web app keeps no other copy, ADR-0098). If it is lost, reset again.
5. Hand it over through a trusted channel, never in a ticket or a shared chat (L-232). Ask the user to keep it in a password manager: there is no way to change it in the app yet (L-233).
6. The user's open sessions were deleted, and a login that was checking the old password at that moment is refused too (ADR-0098); they log in with their email and the temporary password.

The system admin's own password is not reset here: change the hash in the `maiak-admin` Secret (`maiakctl hashpw`, or delete the generated Secrets and re-sync, ADR-0091); the api replaces it at the next start and deletes the admin's sessions.

## Suspend a tenant
Use it for abuse, non-payment or a customer's request to freeze an account.
- **Logins:** a user of the tenant who enters the right password gets "account suspended" (403); a wrong password still gets the generic error.
- **Sessions:** existing sessions stop working on the next request (they are kept; unsuspending revives those that have not expired).
- **Checks and alerts:** the worker claims none of its monitors, its heartbeats are not evaluated and pings are answered 204 but not recorded, and its Slack deliveries are not sent. Deliveries that were already queued wait; see the unsuspend note below. Open incidents stay open.
- **Data:** nothing is deleted; monitors, results, incidents, Slack settings and users stay. Retention keeps working as for everybody.
- Unsuspend with the same button. Checks resume within about a minute. Slack deliveries that waited more than 15 minutes are not sent: the unsuspend marks them `failed` with `last_error = 'tenant_suspended'` and the audit row `admin.tenant.unsuspended` records how many (`deliveries_expired`); newer ones are sent at once (ADR-0098). Pings sent during the suspension were not recorded, so a heartbeat whose deadline passed meanwhile goes down at the next evaluation (within 30 s) and alerts, unless a ping arrives first (L-235); warn the customer. The incident resolves with the next ping.

## Delete a tenant
- Soft delete: `deleted_at` is set and the status becomes `suspended`, with every effect of a suspension. Its pending Slack deliveries are marked `failed` (`last_error = 'tenant_deleted'`) by the next delivery claim and are never sent, even after a restore. The tenant disappears from the default list (tick "Show deleted" to see it) and can no longer be changed in the panel.
- Nothing is erased (L-234). Its users still exist and keep their email: they cannot register again with it, and a login with the right password says "account suspended". To give a deleted customer access again, restore the tenant with SQL as `maiak_owner` (`UPDATE tenants SET deleted_at = NULL, status = 'active', updated_at = now() WHERE id = ...`) and record why in the ledger or the incident note.

## Plans and existing usage
- A plan change applies at once to new creations only. Existing monitors, heartbeats and the Slack channel are never deleted or paused by a downgrade.
- When usage is above the new limit, the customer keeps everything, can edit, pause and delete, and gets "plan limit reached" (409) when creating more; `GET /api/v1/usage` (and the web app) shows usage above the limit. Tell the customer which monitors to delete if they should get under the limit.
- Per-tenant overrides in `tenants.limits` still win over the plan (ADR-0093); the panel does not edit them.
- A plan change and a monitor creation of the same tenant are serialised by the tenant row lock, so a create cannot slip past a downgrade.

## Audit
Every change writes one `audit_log` row into the target tenant with the admin's user id, the client IP and the before/after values; never a password or a hash. There is no viewer in the panel yet (L-230); read it as `maiak_owner`:

```sql
SELECT created_at, action, entity_type, entity_id, metadata, host(ip)
FROM audit_log WHERE action LIKE 'admin.%' ORDER BY created_at DESC LIMIT 50;
```

`audit_log` forces RLS for its owner too: run the query in a transaction after `SELECT set_config('app.tenant_id', '<tenant id>', true);` for one tenant, or as the RDS admin login for all of them.
