# Runbook: first install on an empty database

Use this page when Maiak starts on an **empty database**: the production reinstall on `klr-platform` (O-03), a new environment, or a dedicated RDS (L-009). The very first deploy of the chart (pull secret, DNS, ArgoCD Application) is in [first-deploy.md](first-deploy.md); this page assumes the Application `maiak` exists and the images pull.

Conventions: always pass an explicit `--context <klr-platform context>` to `kubectl` and `--profile <platform profile>` to `aws` (R-006). ArgoCD changes the cluster; never `kubectl apply` or `edit` in namespace `maiak`. Never print secret values: list keys only. Do not edit the `image.tag` / `web.image.tag` lines of `values-platform.yaml` (the release workflow owns them).

## What is different on an empty database
- **Order on klr-platform.** The release with the T-02 contract migration (`20261106200000_projects_contract.sql`) and the schema guard (`20261109090000_schema_version_guard.sql`) is first deployed the normal way, on the **existing** production database, before the reinstall. That is fine: the contract migration is compatible with the previous release (every release since T-02 sets `project_id` on each monitor insert), and its backfill and stray-link clean-up are written for existing data. Only an image from before T-02 breaks on it, and an image rollback below T-02 is unsupported. Then the database is emptied (section 1) and installed again (sections 2 to 7).
- **Schema guard (O-06).** api, worker and jobs refuse to start when the database has a migration newer than their binary (`database schema is newer than this binary` in the pod log). An image rollback is therefore possible only to a release that contains the database's newest migration; rolling back across a migration is not supported (L-311). Images built before O-06 lack the guard: never deploy one by hand. With the guard in place the T-01 rollback hazard is closed (L-282) and granting `owner` in `default` is safe.
- **Status page.** The renderer is enabled in the same commit as the db bootstrap (L-292): bootstrap (-3) creates `maiak_statuspage` and the key `statuspage_url`, migrate (-1) grants `status_page_snapshot(text)`, the renderer starts at wave 0. This one-commit order is equally safe on an existing database ([status-page.md](status-page.md)).
- **Leftovers of the T-02 rollout** (L-286 items 1 and 4, organization-wide channels from the pre-T-02 api) disappear with the old database.

## 1. Empty the existing database (reinstall only)
Done by the owner, who holds the RDS master login (`platform/grafana/db`, Grafana's own login on the shared instance, R-009). It deletes every organization, monitor, result and audit row of Maiak; Grafana's databases on the same instance are not touched. Skip this section for a new environment.
1. **Stop the writers.** Commit to `values-platform.yaml` and let ArgoCD sync:
   ```yaml
   api: {replicas: 0}
   worker: {replicas: 0}
   jobs: {replicas: 0}
   statuspage: {replicas: 0}   # only if enabled
   ```
   Then pause automated sync of the Application `maiak` (ArgoCD UI: App details → Sync policy → disable auto-sync; or remove the `automated` block in `devops-charts-infra`), so nothing restarts the pods while the database is gone. Check (read-only): `kubectl --context <klr-platform context> -n maiak get deploy` shows 0/0 for api, worker and jobs.
2. **Read-only checks first**, from a host inside the VPC or over the VPN that reaches the RDS endpoint (`dbBootstrap.host`). Read the login into the environment without printing it:
   ```sh
   export PGHOST=<RDS endpoint> PGPORT=5432 PGDATABASE=postgres PGSSLMODE=require
   export PGUSER=$(aws --profile <platform profile> secretsmanager get-secret-value --secret-id platform/grafana/db --query SecretString --output text | jq -r .username)
   export PGPASSWORD=$(aws --profile <platform profile> secretsmanager get-secret-value --secret-id platform/grafana/db --query SecretString --output text | jq -r .password)
   psql -c "SELECT current_setting('server_version'), inet_server_addr()"            # the instance you expect
   psql -c "SELECT datname, pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'maiak'"   # owner maiak_owner
   psql -c "SELECT usename, application_name, count(*) FROM pg_stat_activity WHERE datname = 'maiak' GROUP BY 1, 2"   # expect no maiak-* clients
   ```
   Take a snapshot or `pg_dump` first if anything in it may be needed later.
3. **Drop it** (**writes**, irreversible). The master login is not a member of `maiak_owner` after the bootstrap, so it joins the role for the drop only:
   ```sh
   psql -v ON_ERROR_STOP=1 -c "GRANT maiak_owner TO CURRENT_USER" -c "DROP DATABASE maiak WITH (FORCE)" -c "REVOKE maiak_owner FROM CURRENT_USER"
   unset PGPASSWORD PGUSER
   ```
   Keep the roles and the Secret `maiak-db`: the bootstrap reuses them. `WITH (FORCE)` ends leftover sessions (PostgreSQL 13 or newer).
4. Continue with section 2. The commit of section 3 also removes the `replicas: 0` overrides; re-enable automated sync after it (or sync once by hand and then re-enable it).

## 2. Before the first sync
1. **Database.** The database `maiak` must not exist (section 1), or be empty. Use the explicit `--context` and `--profile` everywhere.
2. **Secret `maiak-db`.** Not managed by ArgoCD. If it is still there from the previous installation, the bootstrap reuses its passwords (creating or re-passwording the roles) and adds missing keys such as `statuspage_url`; that is fine. Delete it only if you want new role passwords. It must not exist without its `*_password` keys (the Job refuses a Secret it did not write completely).
3. **Generated Secrets** (`secrets.*.source: generate`, ADR-0091): `maiak-admin`, `maiak-crypto` and `maiak-statuspage` are reused if they exist. On an empty database nothing is encrypted yet, so keeping or deleting `maiak-crypto` makes no difference; delete `maiak-admin` if you want a new admin password (the Job then writes a new `maiak-admin-initial`).
4. **AWS Secrets Manager** (account of `klr-platform`, explicit `--profile`); check names only:

   | Secret | Needed when | Properties |
   |---|---|---|
   | `platform/grafana/db` | always for the bootstrap (exists) | `username`, `password` (RDS master login, R-009) |
   | `platform/maiak/oauth` | `oauth.google.enabled` or `oauth.github.enabled` | `google_client_id`, `google_client_secret`, `github_client_id`, `github_client_secret` (only those of enabled providers are read); see [oauth.md](oauth.md) |
   | `platform/maiak/admin` | only `secrets.admin.source: externalSecret` | `login`, `password_hash` ([first-deploy.md](first-deploy.md) section 1) |
   | `platform/maiak/crypto` | only `secrets.crypto.source: externalSecret` | `keys` (JSON string), `active_key_id` |
   | `platform/maiak/statuspage` | only `secrets.statuspage.source: externalSecret` | `cookie_key` (standard base64 of 32 random bytes) |

   ```sh
   aws --profile <platform profile> secretsmanager describe-secret --secret-id platform/maiak/oauth --query Name   # read-only
   ```
5. **OAuth providers** (optional, [oauth.md](oauth.md)): register the redirect URIs `<MAIAK_PUBLIC_URL>/api/v1/auth/oauth/google/callback` and `.../github/callback` exactly. `MAIAK_PUBLIC_URL` is `config.publicUrl` (`https://maiak.klr-ops.net` on klr-platform); change it only together with the providers.

## 3. One commit to `values-platform.yaml`
```yaml
dbBootstrap:
  enabled: true        # creates the database, the four roles and maiak-db (incl. statuspage_url)
statuspage:
  enabled: true        # safe together with the bootstrap above (L-292)
oauth:                 # optional; needs platform/maiak/oauth
  google:
    enabled: true
  github:
    enabled: false
  allowedDomains: []   # MAIAK_OAUTH_ALLOWED_DOMAINS; e.g. [klr.com.ua] to allow claiming that Workspace domain
config:
  registrationEnabled: false   # recommended until the public-domain gates (R-130, L-172)
```
Remove the `replicas: 0` overrides of section 1 in the same commit. Keep `config.passwordLoginEnabled: true` until a provider works (section 6). Commit and push; ArgoCD syncs (re-enable automated sync if you paused it).

## 4. Sync order and what to check (read-only)
| Wave | Resources | Check |
|---|---|---|
| -4 | ExternalSecret `maiak-db-admin` (hook), bootstrap RBAC | `kubectl --context CTX -n maiak get externalsecret maiak-db-admin` is `SecretSynced` |
| -3 | Job `maiak-db-bootstrap` | `kubectl --context CTX -n maiak logs job/maiak-db-bootstrap`: four `maiak_*` roles with `rolsuper=f`, `rolbypassrls=f`, database `maiak` owned by `maiak_owner`; `kubectl --context CTX -n maiak get secret maiak-db -o json \| jq '.data \| keys'` includes `statuspage_url` |
| -3 | Job `maiak-secrets-bootstrap` | its log lists `maiak-admin`, `maiak-crypto`, `maiak-statuspage` as `secret created` or `left untouched` |
| -2 | ExternalSecrets `maiak-oauth` (and any `externalSecret` sources), migrate ServiceAccount | `kubectl --context CTX -n maiak get externalsecret` all `SecretSynced` |
| -1 | Job `maiak-migrate` | `kubectl --context CTX -n maiak logs job/maiak-migrate` applies every migration up to `20261109090000` without error |
| 0 | Deployments `maiak-api`, `maiak-worker`, `maiak-jobs`, `maiak-web`, `maiak-statuspage`, HTTPRoute | `kubectl --context CTX -n maiak get deploy,pods`; `rollout status` for each; no `database schema is newer than this binary` in the api, worker or jobs logs |

A failed wave stops the sync; read the Job log, fix in Git, let ArgoCD retry.

## 5. Admin (owner) bootstrap
At every start the api's `BootstrapAdmin` creates or adopts the user `MAIAK_ADMIN_LOGIN` (`secrets.admin.login`, default `admin`) as owner of the `default` organization and the only platform admin (ADR-0009, ADR-0097). On a fresh install:
1. Read the one-time password yourself, in your own terminal (only if `maiak-admin-initial` was created): `kubectl --context CTX -n maiak get secret maiak-admin-initial -o jsonpath='{.data.password}' | base64 -d; echo`.
2. Sign in at `https://maiak.klr-ops.net/login?admin` (or the normal form while password login is on), store the password in your password manager, then delete the copy: `kubectl --context CTX -n maiak delete secret maiak-admin-initial` (**writes**, R-110).
3. Invite the first people (Settings → Members, [invitations.md](invitations.md)). Further owners in `default` are fine (L-282 closed by the schema guard).

## 6. OAuth
1. Sign in with each enabled provider from a second browser profile through an invitation, or link it to the admin under Settings → Account → Sign-in methods (`/profile`).
2. Domain auto-join: only domains in `oauth.allowedDomains` can be claimed; an owner claims it under Settings → Sign-in domains ([oauth.md](oauth.md)).
3. Only after a provider works: optionally `config.passwordLoginEnabled: false` (the admin keeps `/login?admin`).

## 7. Switch the bootstrap off
Next commit: `dbBootstrap.enabled: false`; keep `statuspage.enabled: true`. Sync. The Secret `maiak-db` persists. Check (read-only) that `maiak-db-admin` is gone: `kubectl --context CTX -n maiak get externalsecret,secret | grep maiak-db-admin` prints nothing. Remove leftover bootstrap RBAC by hand if wanted (L-103). Until this is done the namespace holds the RDS master login (R-009).

## 8. Verification checklist
- [ ] `curl -fsS https://maiak.klr-ops.net/api/v1/health` answers; `curl -fsS https://maiak.klr-ops.net/healthz` prints ok.
- [ ] Admin signs in; `maiak-admin-initial` deleted.
- [ ] A monitor against a public HTTPS site turns `up` within about a minute; a Slack channel's "Send test" works (encryption key present).
- [ ] Each enabled OAuth provider signs in; `maiak_auth_oauth_logins_total{result="failed"}` stays flat.
- [ ] `kubectl --context CTX -n maiak get pods -l app.kubernetes.io/component=statuspage` is Ready; `curl -sI https://maiak.klr-ops.net/status/does-not-exist` answers 404 with a `Content-Security-Policy` header; publish a page and open it ([status-page.md](status-page.md)).
- [ ] Role check as a superuser (read-only): `SELECT has_function_privilege('maiak_statuspage', 'status_page_snapshot(text)', 'EXECUTE');` is true.
- [ ] Schema check: `SELECT attnotnull FROM pg_attribute WHERE attrelid = 'monitors'::regclass AND attname = 'project_id';` is true and `SELECT to_regprocedure('monitors_fill_project()');` is NULL.
- [ ] `dbBootstrap.enabled` is false again and `maiak-db-admin` is gone (section 7).
- [ ] `config.registrationEnabled` is what you intend (R-130).

## Rollback
- **Configuration:** revert the values commit.
- **Application image:** roll forward. The schema guard refuses an image whose newest migration is older than the database's, so an image rollback works only between releases with the same migrations (L-311); below T-02 it is unsupported in any case.
- **A broken install of an empty database:** repeat it. Scale api, worker, jobs (and statuspage) to 0 and pause automated sync as in section 1 step 1, run the read-only checks and the drop of section 1 steps 2 and 3 (explicit `--profile`, master login read into the environment, `GRANT maiak_owner TO CURRENT_USER`, `DROP DATABASE maiak WITH (FORCE)`, `REVOKE`), keep or delete `maiak-db`, then sync again with the bootstrap on (section 3).
