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
- Rotating `MAIAK_ADMIN_EMAIL` disables the previous owner(s), deletes their sessions and writes an audit row.
- The login rate limit reserves an attempt atomically before the argon2 verification and the verification queue has a bounded wait (HTTP 429). It is in memory, per replica. The per-email limit (10 failures per 15 minutes) lets anyone lock the single admin out for a while; accepted for the MVP (L-007).
- Argon2 memory is capped at 256 MiB; the pod limit is small, so concurrent logins are limited to 2.
- `MAIAK_ENV` defaults to `production`.
- 2026-10-20, ADR-0091: by default the admin Secret is generated in the cluster (system login `admin`, one-time password in `maiak-admin-initial`); AWS Secrets Manager is an alternative per Secret. Password reset: delete the admin Secrets and re-sync. Until B-24 the api still requires an email-shaped `MAIAK_ADMIN_EMAIL` (L-152).
