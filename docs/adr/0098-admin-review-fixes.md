# ADR-0098: Admin panel review fixes: login race, temporary password cache, Slack backlog, login tenant

- Status: Accepted
- Date: 2026-10-22
- Amends: ADR-0097, the ADR-0009 amendment of P-02 (login tenant)

## Context
The independent security review of P-04 (task P-09) found four problems. (m1) `Login` verifies the password against a row read in one transaction and inserts the session in a later tenant transaction; a password reset or a rotation of the system admin that committed in between deleted the user's sessions before the new one existed, so a login with the old password kept a valid session. (m2) The web reset mutation kept its result, the temporary password, in the TanStack mutation cache after the panel was hidden. (m3) An unsuspend sent every Slack delivery queued during the suspension at once, possibly days of stale alerts, and pending deliveries of deleted tenants were read by every claim forever. (m4) `auth_login_tenant` preferred an older suspended tenant over a newer active one, so a member of both was told the account is suspended.

## Decision
- **Re-check the credential where the session is created.** `checkPassword` keeps the verified `password_hash` and `updated_at`. The session transaction first re-reads them with `SELECT ... FOR SHARE` (`LockUserCredential`) and refuses with the generic invalid-credentials answer (audited `auth.login.failed`, reason `credential_changed`) when the hash or `updated_at` differ, or the user is no longer active. The lock closes the remaining window: a reset in flight holds the row lock, so the re-read waits and sees the new row; a reset that starts later waits for the session transaction and its `auth_revoke_user_sessions` then deletes the new session. `admin_reset_user_password`, the bootstrap rotation (`UpdateUserPasswordHash`), `DisableUser` and `ActivateUser` all set `updated_at`. No new grant and no new definer function: `maiak_api` can already read `users`, and its column `UPDATE` grant allows the row lock. The integration test pauses a login between verification and session through an unexported seam (`afterVerify`, a no-op in production) and runs a rotation, and a reset held open in a second transaction.
- **No copy of the temporary password in the mutation cache.** The reset mutation uses `gcTime: 0`, copies the password into component state in `onSuccess` and calls `reset()` at once; hiding the panel clears the state and resets again, leaving the tenant unmounts the component. A test inspects `queryClient.getMutationCache().getAll()`. The password still passed through the HTTP response and React memory; no browser code can wipe those, so ADR-0097's "component state only" is now stated with that limit.
- **Expire the Slack backlog on unsuspend.** `admin_set_tenant_status` (same signature) marks the tenant's `pending` deliveries created more than 15 minutes ago `failed` with `last_error = 'tenant_suspended'` in the same transaction as the status change and adds `deliveries_expired` to the `admin.tenant.unsuspended` audit metadata. Newer deliveries are still sent, keeping the per-monitor order. 15 minutes is a constant (L-240).
- **Fail deliveries of deleted tenants once, in the claim.** `claim_deliveries` (same signature) marks `pending` deliveries of deleted tenants `failed` with `last_error = 'tenant_deleted'` before it picks ready rows (`FOR UPDATE SKIP LOCKED`, like the lease expiry). The claim was chosen over a housekeeping task because it needs no new definer function, no new Go code and catches rows whose lease expires after the deletion. Suspended tenants still wait.
- **Login tenant order.** `auth_login_tenant` (same signature) orders live before deleted, then active before suspended, then `default`, then the oldest membership. `admin_reset_user_password` now takes its audit tenant from `auth_login_tenant`, so both stay in step.
- One forward-only migration, `20261022090000_admin_review_fixes.sql`, with only `CREATE OR REPLACE` of existing functions: owners, pinned `search_path`, grants and the schema-lint allow-list are unchanged.

## Consequences
- During the rolling update an api pod of the previous release still has the login race; the window closes when the rollout finishes.
- A login now holds a share lock on the user row for the length of the session transaction; a reset of the same user waits for it (milliseconds).
- A customer whose tenant is unsuspended does not get alerts older than 15 minutes; open incidents stay visible in the dashboard. Heartbeats still alert at once after an unsuspend (L-235, updated).
- Ledger: L-240 (fixed expiry window and the cleanup inside every claim); L-235 updated.

## Alternatives considered
- **A definer function that locks and compares the credential.** Not needed: the api can lock the row with its existing column `UPDATE` grant, and a function would add an allow-list entry for no extra protection.
- **A credential version column.** Comparing the hash and `updated_at` detects every change the code makes without a schema change; a version column would need an expand/contract migration and changes to every writer.
- **Expiring the backlog at suspension or in the claim.** At suspension the deliveries are still current; in the claim the age of a delivery says nothing about the suspension. The unsuspend is the moment the backlog becomes stale.
- **A jobs housekeeping task for deleted tenants.** It would need a new definer function, its allow-list and grant tests, and a schedule, for the same `UPDATE`.
