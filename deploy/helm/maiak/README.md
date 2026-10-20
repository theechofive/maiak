# maiak Helm chart

Deploys Maiak (multi-tenant uptime monitoring): `api`, `worker`, `jobs` (all from the single
`maiak-backend` image, different `command`), an optional `web` frontend, a migration hook Job,
ExternalSecrets and a Gateway API `HTTPRoute`. No Ingress objects are created.

## Files

- `values.yaml` generic defaults; `values-platform.yaml` klr-platform overrides.
- `image.tag` in `values-platform.yaml` is rewritten by `.github/workflows/release.yml`. Do not hand-edit.

## Render / lint

```sh
helm lint deploy/helm/maiak -f deploy/helm/maiak/values-platform.yaml
helm template maiak deploy/helm/maiak -n maiak -f deploy/helm/maiak/values-platform.yaml
helm template maiak deploy/helm/maiak -n maiak -f deploy/helm/maiak/values-platform.yaml --set web.enabled=true
```

## Secrets (ExternalSecrets, `ClusterSecretStore aws-secrets`)

| Remote key (values-driven) | Properties | K8s Secret |
|---|---|---|
| `platform/maiak/db` | `api_url`, `worker_url`, `owner_url` | `maiak-db` |
| `platform/maiak/admin` | `email`, `password_hash` | `maiak-admin` |
| `platform/maiak/crypto` | `keys` (JSON string), `active_key_id` | `maiak-crypto` |

`MAIAK_DATABASE_URL` is mapped per workload via `<workload>.databaseUrlKey`
(api -> `api_url`, worker/jobs -> `worker_url`); the migrate Job gets `MAIAK_MIGRATE_DATABASE_URL` from `owner_url`.

## Migration hook and first deploy

`maiak-migrate` is an ArgoCD `PreSync` hook. On the very first sync the ExternalSecrets do not exist yet, so the
Job cannot start. Either sync the ExternalSecrets first (selective sync), or deploy once with
`migration.hookPhase=Sync` (Job runs in wave 1, ExternalSecrets in wave -1).

## Open items

- `imagePullSecrets` is empty: ghcr pull-secret decision is open (ledger L-002), blocks first deploy.
- `networkPolicy` is off: the cluster CNI does not enforce NetworkPolicy (risk R-002).
- `serviceMonitor` is off (ledger L-003).
- The worker must never use `hostNetwork` (IMDS hop-limit protection assumes it).
