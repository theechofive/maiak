# Runbook: first deploy to klr-platform

Status: **nothing has been deployed.** The agents that wrote the chart, the workflows and this page did not run anything against a cluster or AWS, and did not push. Every command below is for the owner to run; the commands that read are marked "read-only", the few that write are marked **writes**. Decisions: [ADR-0010](../adr/0010-deployment.md), [ADR-0014](../adr/0014-db-bootstrap-automation.md). Chart: `deploy/helm/maiak` (see its README for the env contract).

Conventions for every command:
- Always pass an explicit `--context <klr-platform context>` to `kubectl` and `--profile <platform profile>` to `aws`. The default context on the developer machine may be a production cluster (R-006).
- Never paste secret values into a terminal history, chat, ticket or Git. Secrets live in AWS Secrets Manager; the commands below write them from a file with mode 0600 and delete it.
- ArgoCD changes the cluster; do not `kubectl apply` or `kubectl edit` anything in namespace `maiak`.

## 0. Gate: do not register the Application before these are true

1. Images exist and the values file points at them. `values-platform.yaml` ships the placeholder tag `"0000000"` for `image.tag` and `web.image.tag`. `release.yml` (never executed yet, L-102, R-010) must have built `ghcr.io/theechofive/maiak-backend:<sha>` and `maiak-web:<sha>` and committed the real tags. With the placeholder the first sync ends in `ImagePullBackOff`.
2. The pull-secret decision (section 2) is made and implemented.
3. The two Secrets Manager secrets (section 1) exist.
4. The DNS record (section 3) exists or you accept that the host does not resolve yet.

## 1. Secrets in AWS Secrets Manager

Region and account: those of `klr-platform`. `platform/grafana/db` already exists (the bootstrap Job reads the RDS master login from it; there is no copy). Two secrets are new. The names are the chart defaults (`externalSecrets.admin.remoteKey`, `externalSecrets.crypto.remoteKey`); the `ClusterSecretStore` is `aws-secrets`.

### `platform/maiak/admin`

JSON shape (exactly two properties):

```json
{ "email": "admin@example.com", "password_hash": "$argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>" }
```

`email` must be a plain address (no display name). `password_hash` is an argon2id PHC string produced by `maiakctl hashpw`, which prompts twice without echo on a terminal (pipe mode reads one line from stdin and would leave the password in your history, so use the prompt). Two ways to run it, pick one:

```sh
# a) with Go, from a checkout of this repository
cd backend && go run ./cmd/maiakctl hashpw
# b) from the image (needs the pull access of section 2; the image is distroless, so call the binary directly)
docker run --rm -it --entrypoint /app/maiakctl ghcr.io/theechofive/maiak-backend:<sha> hashpw
```

Write the secret from a file (**writes**):

```sh
umask 077
printf 'Paste the hash, then Enter: ' && IFS= read -r HASH
jq -n --arg e 'admin@example.com' --arg h "$HASH" '{email:$e,password_hash:$h}' > /tmp/maiak-admin.json
aws --profile <platform profile> secretsmanager create-secret --name platform/maiak/admin --secret-string file:///tmp/maiak-admin.json
rm -f /tmp/maiak-admin.json; unset HASH
```

### `platform/maiak/crypto`

JSON shape: `keys` is a **string containing JSON**, not a nested object, mapping key id to standard base64 of 32 random bytes; `active_key_id` names the key used for new encryptions. Key ids are 1-32 characters of `[A-Za-z0-9_-]`.

```json
{ "keys": "{\"k1\":\"<base64 of 32 bytes>\"}", "active_key_id": "k1" }
```

Generate and store it (**writes**):

```sh
umask 077
KEY=$(openssl rand -base64 32)
jq -n --arg k "$KEY" '{keys: ({"k1": $k} | tojson), active_key_id: "k1"}' > /tmp/maiak-crypto.json
aws --profile <platform profile> secretsmanager create-secret --name platform/maiak/crypto --secret-string file:///tmp/maiak-crypto.json
rm -f /tmp/maiak-crypto.json; unset KEY
```

Losing a key makes the Slack webhook URL stored in the database unreadable, so keep the secret versions (do not delete the secret). Rotation: add `k2` to `keys`, set `active_key_id` to `k2`, keep `k1` until nothing is encrypted with it; the app reads the new value at the next restart (ExternalSecret `refreshInterval` is `"0"`, so the Kubernetes Secret is created once and not refreshed: delete the Secret `maiak-crypto` to make ESO recreate it, then restart api and jobs).

## 2. Decision L-002: how the cluster pulls `ghcr.io/theechofive/maiak-*`

The chart renders no pull secret (`imagePullSecrets: []`) and nothing in the cluster is configured for ghcr. A first sync without one of the options below fails with `ImagePullBackOff` at the bootstrap Job, the very first pod. Options:

| Option | What to do | Consequences |
|---|---|---|
| A. Pull secret from Secrets Manager | Create a GitHub PAT with only `read:packages`, store a dockerconfigjson in `platform/maiak/ghcr`, add an ExternalSecret (type `kubernetes.io/dockerconfigjson`, wave -5 so it exists before the hooks) and set `imagePullSecrets: [{name: ghcr-pull}]` in `values-platform.yaml`. The chart has no template for this yet: it is a small addition plus a ledger update. | Works with private images. The PAT belongs to a person, expires, and must be rotated; a leaked PAT reads every package of that account. |
| B. ECR in the platform AWS account | CI pushes to ECR (GitHub OIDC role), `image.repository` and `web.image.repository` point at ECR, nodes pull with their instance role. No pull secret. | Cleanest at runtime, no long-lived token. Needs AWS roles, an ECR repository and a change to `release.yml`; costs a little storage. |
| C. Public images | After the first push, set both packages (`maiak-backend`, `maiak-web`) to public in GitHub (package settings; the workflow token cannot do it). | Zero cluster work. Anyone can pull the binaries, the embedded SQL migrations and the web bundle. Acceptable only if the repository content may be public; no secret is baked into images. |

Record the choice in `docs/LEDGER.md` (close L-002) and in the ADR if it changes the chart.

## 3. DNS: Cloudflare CNAME to the internal NLB

`maiak.klr-ops.net` is served by the internal Envoy Gateway (`envoy-gateway-internal`, wildcard certificate for `*.klr-ops.net`, listener `https`). Read the NLB hostname without changing anything (read-only):

```sh
kubectl --context <klr-platform context> -n envoy-gateway get gateway envoy-gateway-internal \
  -o jsonpath='{.status.addresses[*].value}{"\n"}'
kubectl --context <klr-platform context> -n envoy-gateway get gateway envoy-gateway-internal \
  -o jsonpath='{range .status.listeners[*]}{.name}{" attached="}{.attachedRoutes}{"\n"}{end}'
```

In Cloudflare add `CNAME maiak.klr-ops.net -> <that hostname>`, **DNS only** (grey cloud: the NLB is internal, a proxied record cannot reach it). Copy the value from the command, do not retype it. The record resolves only inside the VPC or over the VPN.

## 4. ArgoCD Application (added by the owner in `devops-charts-infra`)

Add this manifest to the infra repository in the folder ArgoCD watches for Applications. Adjust `project` to the project your other Applications use and `destination.server` if ArgoCD manages `klr-platform` from another cluster. The Application name must stay `maiak`: the chart derives all resource names (`maiak-api`, `maiak-db-bootstrap`, ...) from the release name, which `releaseName` pins.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: maiak
  namespace: argocd
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default                       # use the project of the other platform Applications
  source:
    repoURL: git@github.com:theechofive/maiak.git
    targetRevision: main
    path: deploy/helm/maiak
    helm:
      releaseName: maiak
      valueFiles:
        - values-platform.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: maiak
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
```

Notes:
- **Repository credential.** The repository is private, so ArgoCD needs a read-only credential for `git@github.com:theechofive/maiak.git`: a GitHub deploy key (read-only) registered as an ArgoCD repository Secret (label `argocd.argoproj.io/secret-type: repository`, fields `type: git`, `url`, `sshPrivateKey`) in the `argocd` namespace, managed the way the infra repository manages its other credentials (ExternalSecret preferred). Without it the Application shows `ComparisonError: authentication required` and nothing is created. The key never goes into this repository.
- **First sync by hand (optional).** `automated` starts syncing as soon as the Application is merged. To watch each wave, merge it without the `automated` block, sync from the ArgoCD UI, and add `automated` afterwards. Keep `selfHeal` only after the first successful sync.
- ExternalSecret objects can show as OutOfSync because ESO defaults extra fields; `ServerSideApply` normally hides this. If it persists add an `ignoreDifferences` entry for `ExternalSecret` `/spec/data/*/remoteRef` defaults (unverified, L-022).

## 5. First sync: order and what to verify

Sync waves (all hooks are `Sync` hooks, so the very first sync works; the hook Jobs have `BeforeHookCreation`):

| Wave | Resources | Verify (read-only, always with `--context`) |
|---|---|---|
| -4 | ExternalSecret `maiak-db-admin` (hook, deleted on success), bootstrap ServiceAccount, Role, RoleBinding | `kubectl --context CTX -n maiak get externalsecret maiak-db-admin` is `SecretSynced`; `get sa,role,rolebinding -l app.kubernetes.io/component=db-bootstrap` |
| -3 | Job `maiak-db-bootstrap` | `kubectl --context CTX -n maiak logs job/maiak-db-bootstrap` shows three `maiak_*` roles with `rolsuper=f`, `rolbypassrls=f` and database `maiak` owned by `maiak_owner`; `kubectl --context CTX -n maiak get secret maiak-db -o json \| jq '.data \| keys'` lists `api_password api_url owner_password owner_url worker_password worker_url` (keys only: never print values); `get secret maiak-db-admin` must be NotFound after the Job (L-022) |
| -2 | ExternalSecrets `maiak-admin`, `maiak-crypto`; migrate ServiceAccount | `kubectl --context CTX -n maiak get externalsecret` both `SecretSynced`; `get secret maiak-admin maiak-crypto -o json \| jq '.data \| keys'` shows `email password_hash` and `active_key_id keys` per secret |
| -1 | Job `maiak-migrate` (owner role) | `kubectl --context CTX -n maiak logs job/maiak-migrate` ends with the applied goose versions and no error |
| 0 | ServiceAccounts, Services, Deployments `maiak-api`, `maiak-worker`, `maiak-jobs`, `maiak-web`, HTTPRoute `maiak` | `kubectl --context CTX -n maiak get deploy,pods`; `kubectl --context CTX -n maiak rollout status deploy/maiak-api --timeout=120s` (same for worker, jobs, web); `kubectl --context CTX -n maiak get httproute maiak -o jsonpath='{.status.parents[*].conditions[*].type}{"\n"}'` shows `Accepted` and `ResolvedRefs` |

If a wave fails ArgoCD stops the sync and leaves the previous workloads running. Read the Job logs and `kubectl --context CTX -n maiak describe job <name>`; fix in Git and let ArgoCD retry. Do not delete Jobs by hand unless you know why.

Also check after wave 0: `kubectl --context CTX -n maiak get pods -o wide` (all `Running`, no restarts), and `kubectl --context CTX top pods -n maiak` if metrics-server exists (memory budget, R-003).

## 6. Verify the application

From inside the VPC or over the VPN (the record is internal):

```sh
curl -fsS https://maiak.klr-ops.net/api/v1/health      # JSON with build info; does not touch the database
curl -fsS https://maiak.klr-ops.net/healthz            # answered by nginx in the web pod (route "/"), prints ok
```

Open `https://maiak.klr-ops.net/` in a browser, sign in with the admin email from `platform/maiak/admin` and the password you hashed, create a monitor against a public HTTPS site, and watch its status turn `up` within about a minute (worker), then set the Slack webhook in Settings and use "Send test" (api, needs the encryption keys). The logs show the worker claiming checks (`kubectl --context CTX -n maiak logs deploy/maiak-worker --tail=20`); metrics stay on the admin port 9090 and are not routed (self-monitoring is L-003).

The web UI has never run against the real API (L-052) and the nginx image has never run read-only in a cluster (L-023); a blank page or a CSP error in the browser console are the first things to look at.

## 7. After the first success: switch the bootstrap off

The bootstrap Job holds the RDS master login while enabled (R-009). Set `dbBootstrap.enabled: false` in `deploy/helm/maiak/values-platform.yaml`, commit, let ArgoCD sync. The Secret `maiak-db` persists because it is not managed by Helm or ArgoCD. Then verify (read-only):

```sh
kubectl --context CTX -n maiak get externalsecret,secret | grep maiak-db-admin   # expect nothing
kubectl --context CTX -n maiak get sa,role,rolebinding -l app.kubernetes.io/component=db-bootstrap
```

The bootstrap ServiceAccount, Role and RoleBinding were created as hooks and ArgoCD does not prune hook resources that disappear from Git, so they probably remain (L-103). They carry no secret and no pod uses them, but the Role can create Secrets in `maiak`; the owner removes them by hand (not an ArgoCD-managed change) once the first deploy is confirmed. Re-enable the flag only for a new database, a new environment or the dedicated RDS (L-009), as described in [database-bootstrap.md](database-bootstrap.md).

## 8. Rollback

- **Application version:** revert the CI bump commit (`chore(deploy): maiak-backend <sha> [skip ci]`) or set the previous tags in `values-platform.yaml` and push; ArgoCD rolls the Deployments back. The schema is not rolled back: migrations are forward-only and backward compatible with the previous version, except the known case L-070 (an old api pod cannot resume a monitor after the grant migration).
- **A bad migration:** fix forward with a new migration ([migrations.md](migrations.md)); the failed Job stops the sync and old pods keep serving.
- **Bad configuration or secret:** correct the Secrets Manager value, delete the Kubernetes Secret created from it so ESO recreates it (`refreshInterval` is `"0"`), restart the Deployments through ArgoCD (change an annotation in Git) rather than editing live objects.
- **Whole app off:** set the replicas to 0 in `values-platform.yaml`, or delete the Application (the finalizer prunes everything it created; the Secret `maiak-db` and the database remain).

## 9. Known risks for this deploy

- **R-009** the bootstrap Job holds Grafana's RDS master login while `dbBootstrap.enabled` is true, and the image comes from a personal GitHub repository. Disable it right after the first success (section 7).
- **L-022** unverified at the first sync: that ArgoCD deletes `maiak-db-admin` after the Job (`HookSucceeded`), that the bootstrap RBAC is sufficient, that ESO resolves `platform/grafana/db`, and the real RDS path of the bootstrap (tested only with a superuser and a `CREATEROLE CREATEDB` approximation).
- **L-023** the web image was never built or run: nginx as uid 65532 with a read-only root filesystem and only `/tmp` writable, the `/healthz` probes, the CSP.
- **L-002** the pull secret (section 2) blocks the first pod.
- **L-102 / R-010 / R-008** the release workflow has never run; the bump commit needs a `main` without branch protection for the bot.
- **R-001** a failure or maintenance window of the shared RDS `grafana` takes Maiak and Grafana down together; **R-003** memory requests total 512Mi with the platform replicas, on a memory-tight cluster; **R-002** NetworkPolicy is not enforced (see L-100 for extra blocked CIDRs); **L-052** the SPA has never run against the real API.
