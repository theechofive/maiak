# maiak Helm chart

Deploys Maiak (multi-tenant uptime monitoring): `api`, `worker`, `jobs` (all from the single
`maiak-backend` image, different `command`), an optional `web` frontend, a migration hook Job,
a DB bootstrap hook Job, a secrets bootstrap hook Job, ExternalSecrets and a Gateway API `HTTPRoute`. No Ingress objects are created.

## Files

- `values.yaml` generic defaults; `values-platform.yaml` klr-platform overrides.
- `image.tag` in `values-platform.yaml` is rewritten by `.github/workflows/release.yml`. Do not hand-edit.

## Render / lint

```sh
bash scripts/validate-chart.sh   # everything below plus invariants and kubeconform
helm lint deploy/helm/maiak -f deploy/helm/maiak/values-platform.yaml
helm template maiak deploy/helm/maiak -n maiak -f deploy/helm/maiak/values-platform.yaml
helm template maiak deploy/helm/maiak -n maiak -f deploy/helm/maiak/values-platform.yaml --set dbBootstrap.enabled=false
helm template maiak deploy/helm/maiak -n maiak -f deploy/helm/maiak/values-platform.yaml --set web.enabled=false
```

`values.schema.json` rejects, among others, a `config.publicUrl` that is not https or ends with a slash and a
workload without `resources.requests` and `resources.limits`. `scripts/check-rendered-chart.py` asserts the
invariants listed under "Environment per workload" and "Hardening" on every render.

## Environment per workload

Source of truth: `backend/internal/platform/config/config.go`.

| Variable | api | worker | jobs | migrate | db-bootstrap |
|---|---|---|---|---|---|
| `MAIAK_ENV`, `MAIAK_LOG_LEVEL` | x | x | x | x | x |
| `MAIAK_ADMIN_ADDR`, `MAIAK_DATABASE_URL` (`api_url` / `worker_url`) | x | x | x | | |
| `MAIAK_ENCRYPTION_KEYS`, `MAIAK_ENCRYPTION_ACTIVE_KEY_ID` (`maiak-crypto`) | x | **never** | x | | |
| `MAIAK_ADMIN_LOGIN`, `MAIAK_ADMIN_PASSWORD_HASH` (`maiak-admin`) | x | **never** | **never** | | |
| `MAIAK_HTTP_ADDR`, `MAIAK_SESSION_TTL`, `MAIAK_COOKIE_SECURE`, `MAIAK_TRUSTED_PROXY_HOPS` | x | | | | |
| `MAIAK_PUBLIC_URL` (`config.publicUrl`, default `https://maiak.klr-ops.net`) | x | | x | | |
| `MAIAK_BLOCKED_CIDRS` (`config.blockedCIDRs`, only rendered when non-empty) | x | x | x | | |
| `MAIAK_WORKER_CLAIM_BATCH`, `_CONCURRENCY`, `_TENANT_CONCURRENCY`, `_HOST_CONCURRENCY` | | x | | | |
| `MAIAK_WORKER_LOCATION` (`worker.location`, default `home-eu-central-1`) | | x | | | |
| `MAIAK_MIGRATE_DATABASE_URL` (`owner_url`) | | | | x | |
| `MAIAK_BOOTSTRAP_*` (admin login from the hook ExternalSecret) | | | | | x |

The secrets bootstrap Job gets only `MAIAK_ENV`, `MAIAK_LOG_LEVEL` and `MAIAK_SECRETS_*` (modes, admin login,
Secret names, key id): no database URL and no RDS login.

Only the migrate Job reads `owner_url`.

## Hardening and resources

- Every container: non-root, read-only root filesystem, `allowPrivilegeEscalation: false`, drop ALL, seccomp
  `RuntimeDefault`; no `hostNetwork`. Only the db-bootstrap ServiceAccount and Job mount a token.
- `terminationGracePeriodSeconds`: 45 for worker and jobs (ADR-0022, ADR-0051), 30 for api and web.
- Probes: api, worker and jobs use `/healthz` and `/readyz` on the admin port; web uses `/healthz` on 8080 (served
  by nginx itself). Web mounts only `/tmp` (emptyDir) because `web/nginx.conf` keeps the pid and temp files there.
- Requests (cpu/memory): api 50m/96Mi, worker 50m/96Mi, jobs 25m/64Mi, web 10m/32Mi; memory limits are 4x. With the
  platform replicas (2, 2, 1, 2) the memory requests total 512Mi; `klr-platform` is memory-tight (R-003), so raise
  replicas or requests deliberately.
- Optional and off: `api.pdb`, `serviceMonitor` (L-003), `networkPolicy` (R-002), `topologySpread`.
- Routing: `/api` goes to the api (60 s request timeout), `/` to the web when `web.enabled`.

## Secrets

Workloads always read `maiak-admin` (`login`, `password_hash`) and `maiak-crypto` (`keys`, `active_key_id`).
`secrets.admin.source` and `secrets.crypto.source` choose, per Secret, who creates them (ADR-0091):

- `generate` (default): the secrets bootstrap hook Job (`/app/maiakctl secrets bootstrap`) creates the Secret
  in the cluster when it is absent and never overwrites it (an incomplete existing Secret fails the Job).
  - admin: login `secrets.admin.login` (default `admin`, stored under the key `login`), a random password whose
    argon2id hash goes to `maiak-admin`; the plaintext goes ONCE to `maiak-admin-initial`
    (`login`, `password`). Read it, log in, delete `maiak-admin-initial`. It is never recreated.
  - crypto: one random 32-byte key with id `secrets.crypto.keyId` (default `k1`). It lives only in the cluster;
    deleting the namespace loses it (it only encrypts the Slack webhook URLs, which can be re-entered).
- `externalSecret`: an ExternalSecret (`ClusterSecretStore aws-secrets`, wave -2) materialises it:

| Remote key (values-driven) | Properties | K8s Secret |
|---|---|---|
| `secrets.admin.remoteKey` (`platform/maiak/admin`) | `login`, `password_hash` | `maiak-admin` |
| `secrets.crypto.remoteKey` (`platform/maiak/crypto`) | `keys` (JSON string), `active_key_id` | `maiak-crypto` |

The secrets Job (`maiak-secrets-bootstrap`, rendered when any source is `generate`) has its own ServiceAccount
with an API token and a Role limited to `create` on secrets and `get,update,patch` on exactly `maiak-admin`,
`maiak-admin-initial` and `maiak-crypto`. It has no database credentials, so unlike `dbBootstrap` it stays enabled.
Password reset: delete `maiak-admin` (and `maiak-admin-initial` if still there) and re-sync.

The Secret `maiak-db` is NOT an ExternalSecret and NOT managed by Helm/ArgoCD: the `dbBootstrap` Job creates it
at runtime with keys `api_url`, `worker_url`, `owner_url`, `owner_password`, `api_password`, `worker_password`
(ADR-0014). With `dbBootstrap.enabled=false` the chart still renders and the migrate Job and workloads assume
`maiak-db` already exists. The first sync order and checks are in `docs/runbooks/first-deploy.md`.

`MAIAK_DATABASE_URL` is mapped per workload via `<workload>.databaseUrlKey`
(api -> `api_url`, worker/jobs -> `worker_url`); the migrate Job gets `MAIAK_MIGRATE_DATABASE_URL` from `owner_url`.

## DB bootstrap hook (`dbBootstrap`)

When `dbBootstrap.enabled=true` (on in `values-platform.yaml` for the first deploy), an ArgoCD Sync hook Job
runs `/app/maiakctl db bootstrap` before the migration: it creates the database and
roles (`maiak_owner`, `maiak_api`, `maiak_worker`) and the Secret `maiak-db`. It reads the RDS master login from
a short-lived ExternalSecret `maiak-db-admin` (`platform/grafana/db`, deleted after success) and uses a
dedicated ServiceAccount `maiak-db-bootstrap` (one of the two in the chart with an API token mounted), limited by a
Role (`create` on secrets; `get,update,patch` only on `maiak-db`).

Security trade-off (risk R-009): while enabled, the Job holds the RDS master login, which is Grafana's own login
on the shared instance. After the first successful deploy set `dbBootstrap.enabled: false` in
`values-platform.yaml` (the Secret persists), or move to a dedicated RDS (L-009).

## Sync waves and first deploy

All hooks are ArgoCD `Sync` hooks ordered by waves, so the very first sync works:

| Wave | Resources |
|---|---|
| -4 | `maiak-db-admin` ExternalSecret, db and secrets bootstrap ServiceAccount/Role/RoleBinding |
| -3 | `maiak-db-bootstrap` and `maiak-secrets-bootstrap` Jobs |
| -2 | `maiak-admin` / `maiak-crypto` ExternalSecrets (only with source `externalSecret`), migrate ServiceAccount |
| -1 | `maiak-migrate` Job |
| 0 | workloads (api, worker, jobs, web), Services, HTTPRoute |

`migration.hookPhase=PreSync` is available only with `dbBootstrap.enabled=false` (rendering fails otherwise) and
would run the Job before the ExternalSecrets exist on a first sync.

## Open items

- `imagePullSecrets` is empty: ghcr pull-secret decision is open (ledger L-002), blocks first deploy.
- `networkPolicy` is off: the cluster CNI does not enforce NetworkPolicy (risk R-002).
- `serviceMonitor` is off (ledger L-003).
- The worker must never use `hostNetwork` (IMDS hop-limit protection assumes it).
