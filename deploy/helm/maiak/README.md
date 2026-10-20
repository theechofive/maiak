# maiak Helm chart

Deploys Maiak (multi-tenant uptime monitoring): `api`, `worker`, `jobs` (all from the single
`maiak-backend` image, different `command`), an optional `web` frontend, a migration hook Job,
a DB bootstrap hook Job, ExternalSecrets and a Gateway API `HTTPRoute`. No Ingress objects are created.

## Files

- `values.yaml` generic defaults; `values-platform.yaml` klr-platform overrides.
- `image.tag` in `values-platform.yaml` is rewritten by `.github/workflows/release.yml`. Do not hand-edit.

## Render / lint

```sh
helm lint deploy/helm/maiak -f deploy/helm/maiak/values-platform.yaml
helm template maiak deploy/helm/maiak -n maiak -f deploy/helm/maiak/values-platform.yaml
helm template maiak deploy/helm/maiak -n maiak -f deploy/helm/maiak/values-platform.yaml --set web.enabled=true
helm template maiak deploy/helm/maiak -n maiak -f deploy/helm/maiak/values-platform.yaml --set dbBootstrap.enabled=false
```

## Secrets

ExternalSecrets (`ClusterSecretStore aws-secrets`):

| Remote key (values-driven) | Properties | K8s Secret |
|---|---|---|
| `platform/maiak/admin` | `email`, `password_hash` | `maiak-admin` |
| `platform/maiak/crypto` | `keys` (JSON string), `active_key_id` | `maiak-crypto` |

The Secret `maiak-db` is NOT an ExternalSecret and NOT managed by Helm/ArgoCD: the `dbBootstrap` Job creates it
at runtime with keys `api_url`, `worker_url`, `owner_url`, `owner_password`, `api_password`, `worker_password`
(ADR-0014). With `dbBootstrap.enabled=false` the chart still renders and the migrate Job and workloads assume
`maiak-db` already exists.

`MAIAK_DATABASE_URL` is mapped per workload via `<workload>.databaseUrlKey`
(api -> `api_url`, worker/jobs -> `worker_url`); the migrate Job gets `MAIAK_MIGRATE_DATABASE_URL` from `owner_url`.

## DB bootstrap hook (`dbBootstrap`)

When `dbBootstrap.enabled=true` (on in `values-platform.yaml` for the first deploy), an ArgoCD Sync hook Job
runs `/app/maiakctl db bootstrap` before the migration: it creates the database and
roles (`maiak_owner`, `maiak_api`, `maiak_worker`) and the Secret `maiak-db`. It reads the RDS master login from
a short-lived ExternalSecret `maiak-db-admin` (`platform/grafana/db`, deleted after success) and uses a
dedicated ServiceAccount `maiak-db-bootstrap`, the only one in the chart with an API token mounted, limited by a
Role (`create` on secrets; `get,update,patch` only on `maiak-db`).

Security trade-off (risk R-009): while enabled, the Job holds the RDS master login, which is Grafana's own login
on the shared instance. After the first successful deploy set `dbBootstrap.enabled: false` in
`values-platform.yaml` (the Secret persists), or move to a dedicated RDS (L-009).

## Sync waves and first deploy

All hooks are ArgoCD `Sync` hooks ordered by waves, so the very first sync works:

| Wave | Resources |
|---|---|
| -4 | `maiak-db-admin` ExternalSecret, bootstrap ServiceAccount/Role/RoleBinding |
| -3 | `maiak-db-bootstrap` Job |
| -2 | `maiak-admin` / `maiak-crypto` ExternalSecrets, migrate ServiceAccount |
| -1 | `maiak-migrate` Job |
| 0 | workloads (api, worker, jobs, web), Services, HTTPRoute |

`migration.hookPhase=PreSync` is available only with `dbBootstrap.enabled=false` (rendering fails otherwise) and
would run the Job before the ExternalSecrets exist on a first sync.

## Open items

- `imagePullSecrets` is empty: ghcr pull-secret decision is open (ledger L-002), blocks first deploy.
- `networkPolicy` is off: the cluster CNI does not enforce NetworkPolicy (risk R-002).
- `serviceMonitor` is off (ledger L-003).
- The worker must never use `hostNetwork` (IMDS hop-limit protection assumes it).
