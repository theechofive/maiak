# ADR-0009: Single admin from a Secret, with a real users table

- Status: Accepted
- Date: 2026-10-20

## Context
The owner is the only user for now. A first plan was to have no users table at all and read the admin only from a Secret.

## Decision
The admin email and an argon2id password hash come from a Kubernetes Secret (fed from AWS Secrets Manager). At startup the api upserts the default tenant, a `users` row for the admin and an `owner` membership. Login creates a server-side session (random token, only its SHA-256 stored) in an HttpOnly, Secure, SameSite=Lax cookie, with a CSRF token for unsafe methods, a login rate limit and audit entries. Resetting the password means changing the Secret. There is no signup, no email sending, no password reset flow. `maiakctl hashpw` generates hashes.

## Consequences
- Sessions and permissions already refer to `user_id`, so adding users later needs no rewrite.
- The in-memory login rate limit is per replica only (tracked in L-007).
- OAuth providers and invitations are deferred (L-007).

## Alternatives considered
- No users table: would force a rewrite of sessions and authorization when users arrive.
- Own signup with email verification: needs SMTP and more attack surface, not needed now.

## Amendments after implementation and review (2026-10-20)
- Sessions: `expires_at` is fixed at login; only `last_seen_at` slides. Login always binds to the `default` tenant.
- Session lookup rejects disabled users and suspended or deleted tenants; a changed admin password hash deletes that user's sessions.
- Rotating `MAIAK_ADMIN_EMAIL` (now `MAIAK_ADMIN_LOGIN`) disables the previous owner(s), deletes their sessions and writes an audit row.
- The login rate limit reserves an attempt atomically before the argon2 verification and the verification queue has a bounded wait (HTTP 429). It is in memory, per replica. The per-email (now per-login) limit (10 failures per 15 minutes) lets anyone lock the single admin out for a while; accepted for the MVP (L-007).
- Argon2 memory is capped at 256 MiB; the pod limit is small, so concurrent logins are limited to 2.
- `MAIAK_ENV` defaults to `production`.
- 2026-10-20, ADR-0091: by default the admin Secret is generated in the cluster (system login `admin`, one-time password in `maiak-admin-initial`); AWS Secrets Manager is an alternative per Secret. Password reset: delete the admin Secrets and re-sync. Until B-24 the api still required an email-shaped `MAIAK_ADMIN_EMAIL` (L-152, closed by B-24 below).

## Amendment: the identifier is a login, not an email (2026-10-20, B-24)
- The admin identifier is a **login**: 1-64 characters of `[a-z0-9._@-]`, default `admin` (a system user, like Grafana). An email address is still a valid login. Logins are case-insensitive: the config (`MAIAK_ADMIN_LOGIN`), `maiakctl secrets bootstrap`, the login request and the login rate-limit key all lowercase it, so `Admin` and `admin` are the same user and the same rate-limit bucket.
- Renamed end to end, before any deploy: env `MAIAK_ADMIN_EMAIL` is now `MAIAK_ADMIN_LOGIN`; the Secret `maiak-admin` holds `login` and `password_hash` (the Secrets Manager JSON for `externalSecret` mode is `{"login": ..., "password_hash": ...}`); the API login body and the `User` object carry `login` instead of `email`. The failure answer stays generic ("Wrong login or password").
- **Column semantics.** The column `users.email` (citext, unique) is kept without a migration and holds the login identifier: `admin` for the system admin, a real email address for users added later (L-007). Code maps it to `login` at the API boundary. Its `CHECK (length(email) BETWEEN 3 AND 320)` means a configured login of 1-2 characters passes config validation but fails the admin bootstrap at api startup (L-160).

## Amendment: registered users next to the system login (2026-10-21, P-02)
- **Registration.** `POST /api/v1/auth/register` (public, no CSRF token, like login) creates in one transaction a tenant on the `free` plan (slug from the email's local part plus 6 random characters, since P-07 a random `t-` slug, ADR-0095; name = the email's domain or `Workspace`), the user, an `owner` membership, an `auth.register` audit row (metadata: the tenant id only) and a session; the answer is the login answer (ADR-0092). No email verification and no outgoing email (L-170). The kill switch is `MAIAK_REGISTRATION_ENABLED` (default true, chart `config.registrationEnabled`); `GET /api/v1/public/config` tells the web app whether to offer it. The original decision "no signup" is superseded.
- **Column semantics.** `users.email` (citext, unique) now holds two kinds of identifiers: the system login of the admin (`admin` by default) and the lowercase email address of each registered user. A registration email must contain one `@`, so it never collides with a system login such as `admin`; an admin login configured as an email address makes that email unavailable for registration (409). The API keeps calling it `login`. The `CHECK (length(email) BETWEEN 3 AND 320)` stays; the app enforces 254.
- **Login alphabet.** A login is 3-254 characters of `[a-z0-9._@+-]`, lowercased everywhere (`config.NormalizeLogin` for `MAIAK_ADMIN_LOGIN` and `maiakctl secrets bootstrap`, the chart schema, the OpenAPI `pattern`/`maxLength`). The minimum of 3 matches the column check and closes L-160. The OpenAPI `minLength` stays 1 so the contract does not narrow; a shorter login simply fails with the generic answer. The registration email policy uses the same alphabet, so every registered email can be used to log in; other characters an RFC 5322 address may contain (`'`, `!`, ...) are rejected with 422.
- **Password policy.** 12-128 Unicode code points, not equal to the email (case-insensitive), no composition rules; hashed with the existing argon2id function and the parameters of the admin hash (the dummy hash uses the same ones, so verification time does not depend on the kind of user). Hashing shares the bounded verification queue of the login (2 slots, bounded wait, then 429).
- **Login binds to the user's tenant.** Login no longer always binds to the `default` tenant: it binds to the user's oldest membership in a tenant that is not deleted, found by the `SECURITY DEFINER` function `auth_login_tenant(user_id)` (executable by `maiak_api` only, reading `memberships` through the owner-only policy `owner_membership_lookup`, both in the schema-lint allow-lists), because the memberships policy hides every row until the tenant is known. A suspended tenant or no membership gives the generic failure. Failed logins of a known user are audited in that user's tenant; unknown logins in the `default` tenant. Choosing among several memberships is deferred (L-190).
- **Rate limits.** No new limiter (L-171). Every registration attempt that reaches the password hash counts against the login's per-client-IP limiter (30 per 15 minutes per replica, shared with failed logins), a success included; policy failures (422) do not count. Duplicate-email answers reveal that an account exists; without verification this cannot be hidden (R-150).

## Amendment: admin adoption and the default tenant (2026-10-21, P-07)
See ADR-0095. `BootstrapAdmin` refuses to start when `MAIAK_ADMIN_LOGIN` names an existing user with any membership outside `default` (`auth_count_other_memberships`), deletes all sessions of an adopted user, of a user whose hash changed and of retired previous owners in every tenant (`auth_revoke_user_sessions`), and `auth_login_tenant` prefers the `default` tenant over older memberships. Tenants are created through `auth_create_tenant`, which picks the plan (`internal` for `default`, `free` otherwise).

## Amendment: platform admin and suspended tenants (2026-10-21, P-04)
See ADR-0097. The configured system admin is the only platform admin (`users.is_platform_admin`, set by `BootstrapAdmin` through `auth_sync_platform_admin` at every start and cleared for a retired admin). A login with the right password for a user whose tenant is suspended or deleted answers 403 `urn:maiak:problem:account-suspended` instead of the generic failure (a wrong password still gets the generic 401); `auth_login_tenant` returns a deleted tenant when the user has no live one. The platform admin resets other users' passwords through the panel (temporary password shown once); the system admin's own password stays in its Secret.
