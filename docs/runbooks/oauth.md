# Runbook: sign-in with Google and GitHub

Decision: [ADR-0104](../adr/0104-oauth-login.md) (O-01, review fixes O-04, web O-02). This page covers how to set up the providers, how to switch to OAuth-only sign-in, and how to recover access.

## How it works for users
- The login page shows one button per configured provider (**Sign in with Google**, **Sign in with GitHub**). If password sign-in is on, the password form is shown below the buttons.
- A user is found by the provider's stable id (Google `sub`, GitHub numeric id), **never by email**. If someone signs in with a provider and an account with the same email already exists, Maiak answers "An account with this email already exists" (`/login?error=account_exists`). The person signs in with the password and links the provider under **Settings → Account → Sign-in methods** (`/profile`).
- A new account is created only through an invitation (the invitation page also offers the provider buttons), through a Google Workspace domain claimed by an organization, or when self-registration is open (`MAIAK_REGISTRATION_ENABLED`). Anyone else gets "There is no Maiak account for this sign-in" (`not_invited`).
- Error codes in `/login?error=<code>` and `/profile?error=<code>` are listed in the spec of `oauthCallback`. The provider's own error text is never shown.

## Redirect URIs
Maiak sends exactly this redirect URI. Register it at the provider character for character: https, no trailing slash, the same host as `MAIAK_PUBLIC_URL` (Helm `config.publicUrl`).

| Provider | Redirect / callback URL |
|---|---|
| Google | `<MAIAK_PUBLIC_URL>/api/v1/auth/oauth/google/callback`, for example `https://maiak.klr-ops.net/api/v1/auth/oauth/google/callback` |
| GitHub | `<MAIAK_PUBLIC_URL>/api/v1/auth/oauth/github/callback` |

If you change `MAIAK_PUBLIC_URL`, update both providers before deploying. Otherwise every sign-in fails at the provider with a redirect URI mismatch.

## Creating the Google OAuth client
1. In Google Cloud Console, use (or create) a project owned by the company's Workspace organization.
2. **APIs & Services → OAuth consent screen**: set the user type to **Internal**. Only accounts of your Workspace can then sign in, and no Google verification is needed. Fill in the app name (Maiak) and the support email. Scopes: `openid`, `email`, `profile` (the defaults; Maiak asks for nothing else).
3. **Credentials → Create credentials → OAuth client ID**, application type **Web application**:
   - Authorized JavaScript origins: none needed.
   - Authorized redirect URIs: the Google URL from the table above.
4. Copy the client id and the client secret into the AWS secret (below). Do not paste them into chats, tickets or values files.

Maiak requires `email_verified` on the Google account and reads `hd` (the Workspace domain). Gmail accounts have no `hd`, so they never join through a domain.

## Creating the GitHub OAuth app
1. In GitHub, go to **Settings → Developer settings → OAuth Apps → New OAuth App**. Create it under the company's GitHub organization if you can, so that it does not depend on one person's account.
2. Homepage URL: `MAIAK_PUBLIC_URL`. Authorization callback URL: the GitHub URL from the table above. Leave device flow off.
3. Generate a client secret and copy the client id and the secret into the AWS secret.

Maiak asks for the scopes `read:user user:email` and uses only the **primary, verified** email from `/user/emails`. The access token is used for those two calls and then discarded. GitHub users join only through an invitation, by linking, or through open registration; there is no domain or organization mapping (L-302).

## The secret `platform/maiak/oauth`
The credentials always come from AWS Secrets Manager through the ExternalSecret `maiak-oauth`; they never go into values files. Create or update the JSON secret in the platform account (always pass an explicit `--profile`):

```json
{
  "google_client_id": "1234-abc.apps.googleusercontent.com",
  "google_client_secret": "…",
  "github_client_id": "Iv1.…",
  "github_client_secret": "…"
}
```

Only the keys of the enabled providers are read. Then enable the providers in `deploy/helm/maiak/values-platform.yaml` and let ArgoCD sync (never `kubectl apply`):

```yaml
oauth:
  google:
    enabled: true
  github:
    enabled: true
```

The ExternalSecret has `refreshInterval: "0"`. After you rotate a secret in AWS, force a refresh of the ExternalSecret (or delete the Secret so that it is recreated) and restart the api pods: they read the variables only at startup. The api refuses to start if a provider has only one of the two values.

## Sign-in domains (Google Workspace auto-join)
The operator decides which domains may be claimed at all: `MAIAK_OAUTH_ALLOWED_DOMAINS` (Helm `oauth.allowedDomains`, lowercase). It is empty by default, which turns claiming and auto-join off. Example for the company domain:

```yaml
oauth:
  allowedDomains: [klr.com.ua]
```

Then an **owner** of the organization:
1. Opens **Settings → Sign-in domains** and chooses **Sign in with Google again**, using a Google account of that domain. This proves control for 10 minutes. If another Google account is already linked to the owner, they first unlink it under **Sign-in methods** (`/profile`), or they use that account. The proof comes back to `/profile`; the Settings link there leads back to the domains section.
2. Enters the domain and chooses **Claim domain**. A new domain starts with the role **viewer**; the owner can raise it to member or admin afterwards.

From then on, a person whose verified Google account has `hd = klr.com.ua` joins that organization on their **first** Google sign-in. Existing accounts do not join (L-305). A domain belongs to one organization only.

Why there is an allow-list (R-214): every employee of a Workspace has the same `hd`, so without it anyone could claim the company domain for an organization of their own. If a domain was claimed by the wrong organization, remove it from `oauth.allowedDomains` and deploy (auto-join stops at once). Then the owner of that organization releases it, or the platform admin deletes that organization. All changes are audited as `org.auth_domain_changed`.

## Going OAuth-only without losing the admin
1. Configure at least one provider and check that people can sign in with it. Have every user link a provider on `/profile` while they still have their password.
2. Set `config.passwordLoginEnabled: false`. The chart and the api refuse this setting if no provider is configured.
3. The password form disappears for everyone. The platform admin (`MAIAK_ADMIN_LOGIN`) keeps it at **`/login?admin`**: this is the break-glass account, so keep its password (Secret `maiak-admin`) somewhere safe.
4. Password registration is refused as well. Invitations then work only through the provider buttons.

## Recovery
- **A user lost access to their provider account** (left the company, GitHub account deleted): OAuth-only users have no password and no self-service recovery (L-303). Remove them from the organization and invite them again with a new provider account. If they still have another linked method, they can sign in with it and unlink the old one on `/profile`.
- **A provider is down or misconfigured.** There are two symptoms:
  - Google's discovery document cannot be loaded and there is no cached copy. The sign-in buttons answer "The provider cannot be reached right now" (503 from the start request), and the api logs `oauth start failed`.
  - Otherwise, the code exchange fails: sign-ins return to `/login?error=failed`, the api logs `oauth code exchange failed`, and `maiak_auth_oauth_logins_total{result="failed"}` rises.

  In both cases, sign in as the admin at `/login?admin`. If the problem is configuration (redirect URI, rotated secret), fix it at the provider or in the secret and restart the api. Temporarily setting `config.passwordLoginEnabled: true` again gives password users their form back.
- **"The sign-in took too long or was started in another browser"** (`state_invalid`): the flow must finish in the same browser within 10 minutes. Rotating the encryption key also ends flows that are in progress (L-304). Start again.
- **Someone cannot unlink a provider** ("This is your only way to sign in"): link another provider first. When password sign-in is off, a password does not count, except for the admin.
- **Too many attempts** (`rate_limited`): failed callbacks share the login limit per client address (30 failures per 15 minutes per api replica). Wait, or check for a misconfigured client causing repeated failures.

## Audit
`auth.oauth_login`, `auth.oauth_failed` (with a short reason code, never the provider's text), `auth.identity_linked`, `auth.identity_unlinked`, `member.joined` (with `via: auth_domain` for domain joins) and `org.auth_domain_changed`.
