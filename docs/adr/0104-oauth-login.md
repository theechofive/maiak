# ADR-0104: OAuth login (Google Workspace, GitHub)

Status: accepted (2026-11-06)

## Context
Colleagues will use one shared installation. Password-only login does not fit a company: no shared identity, no way to join by company domain. `auth_identities` exists since the first migration and is unused (L-007, L-269). The product has no real users yet, so the schema and flows can change freely.

## Decision
- **Providers:** Google (OpenID Connect, Workspace aware) and GitHub (OAuth 2.0). GitLab and generic OIDC are later (L-301). The code has one `Provider` interface (declared in `internal/oauth`, the consumer side) with two implementations; no provider tokens are stored, only the identity (`provider`, `subject`, verified email).
- **Flow:** authorization code with PKCE (S256), `state` and `nonce` in one short-lived (10 min) HMAC-signed `HttpOnly` cookie bound to the browser, exact redirect URI from `MAIAK_PUBLIC_URL`. `POST /api/v1/auth/oauth/{provider}/start` (body: optional `inviteToken`, `intent` login or link) returns the provider URL; `GET /api/v1/auth/oauth/{provider}/callback` finishes the flow, sets the normal session cookie and redirects to `/` (errors redirect to `/login?error=<code>`, never reflecting provider text). The invitation token travels in the POST body, never in a URL.
- **Identity rules:**
  - Google: `email_verified` must be true; the stable key is `sub`. GitHub: the stable key is the numeric user id; only the primary verified address from `/user/emails` is used.
  - A login finds the user by `(provider, subject)`. No match: a new user is created only when (a) an invitation token is presented and valid, or (b) the verified Google `hd` claim equals a domain claimed by an organization with auto-join (below), or (c) self-registration is enabled (`MAIAK_REGISTRATION_ENABLED`). Otherwise the answer is `not_invited`.
  - **No automatic linking to an existing account by email.** Passwords registered with an unverified email (R-120) would allow pre-hijacking: an attacker registers `victim@company` with a password, the victim signs in with Google and lands in the attacker's account. An existing user links a provider only while logged in (`intent: link`, profile page); an OAuth email that belongs to an existing unlinked account answers `account_exists` (sign in with the password, then link). OAuth-created users have `email_verified_at` set.
  - Unlinking is allowed only when another way to sign in remains (password or another identity).
- **Domain auto-join (Google Workspace):** an organization owner claims a domain (`org_auth_domains`: `tenant_id`, `domain`, `default_role` admin/member/viewer with `viewer` default, `enabled`). Joining requires the `hd` claim to match and `email_verified`; a domain can be claimed by one organization only (global unique), and claiming needs the owner to be signed in with a Google account whose `hd` equals the domain (proof of control). Joining adds the user to that organization (and to its Default project through `all_members`). Gmail addresses (no `hd`) never auto-join.
- **Policy switch:** `MAIAK_PASSWORD_LOGIN_ENABLED` (default true) turns off the password form for everyone except the platform admin login (`/login?admin`), so a company can run OAuth-only without losing the break-glass account.
- **Configuration:** `MAIAK_PUBLIC_URL`, `MAIAK_OAUTH_GOOGLE_CLIENT_ID/_SECRET`, `MAIAK_OAUTH_GITHUB_CLIENT_ID/_SECRET`; a provider with no credentials is hidden (`GET /api/v1/auth/providers`, public, lists enabled providers and the password switch). Secrets come from the ExternalSecret `platform/maiak/...`, never from values files.
- **Sessions and rate limits:** the same session creation as password login (a user with several organizations enters the last used or the oldest membership, switcher as before); callbacks share the login rate limiter by IP; every outcome writes an audit row (`auth.oauth_login`, `auth.oauth_failed`, `auth.identity_linked`, `auth.identity_unlinked`, `org.auth_domain_changed`).
- **Authorization:** the new operations are added to `internal/authz` (start and callback public; link and unlink any signed-in user; auth domains owner only), the all-operations test keeps denying unknown ones.

## Consequences
- A shared installation works with company Google accounts and no invitation per person; GitHub works through invitations or linking.
- Members who only have an OAuth identity cannot use the password form; recovery is the identity provider's.
- Not built: SAML, SCIM, group-to-role mapping, GitHub organization membership checks, GitLab (L-301..L-303).
