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
