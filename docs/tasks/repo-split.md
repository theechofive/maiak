# Repository split and rename: inventory

Inventory only (O-08). **Nothing has been moved.** Decision: [ADR-0105](../adr/0105-open-core-and-editions.md). Order of the later tasks: (1) rename the project, mechanically, using section 3; (2) split the repository using sections 1 and 2; (3) choose the core licence (L-313) before the first public release.

Destinations:
- **public**: the public repository (core under AGPL-3.0 or FSL, to be decided; Apache-2.0 where noted).
- **ee**: `ee/` directories in the public repository under the Enterprise licence. Empty today: no paid module exists yet (L-312).
- **private**: the private `maiak-cloud` repository (SaaS operations only).
- **rewrite**: goes public after internal details are removed or replaced by placeholders.
- **delete**: not carried over.

Counts were taken on 2026-11-09 with `git grep` over tracked files (`web/package-lock.json` and `go.sum` excluded). Re-run the greps in section 2 right before the split; the lists change with every commit.

## 1. Files and directories by destination

| Path | Destination | Notes |
|---|---|---|
| `backend/cmd/{api,worker,jobs,migrate,statuspage}` | public | Go module path changes with the rename (section 3) |
| `backend/cmd/maiakctl` | public | Binary name carries the project name; `secrets bootstrap`/`db bootstrap` stay public (self-hosted needs them) |
| `backend/internal/**` | public | Paid modules later go to `backend/ee/**` (build tag `ee`) |
| `backend/internal/platform/config/config.go` | rewrite | Default `MAIAK_WORKER_LOCATION` `home-eu-central-1` names our region; make it neutral (`home`) together with the seed migration's location code |
| `backend/migrations/**` | public | Migrations are immutable once applied. They contain role names (`maiak_*`) and the seeded location `home-eu-central-1` / `aws` / `eu-central-1`; see section 3 for the cost |
| `backend/tools/**`, `backend/go.mod`, `sqlc.yaml`, `Makefile`, `.golangci.yml`, `Dockerfile` | public | Module path `github.com/theechofive/maiak/backend` changes with the rename |
| `api/openapi.yaml` | rewrite | `servers` and `info.contact` point at `maiak.klr-ops.net`, `klr-platform` and `github.com/theechofive/maiak`; replace with placeholders. Problem URNs `urn:maiak:*` (section 3) |
| generated client (`web/src/api/generated`, future Go/TS client package) | public, Apache-2.0 | Regenerated from the spec |
| `web/**` (app, kit, tests, `Dockerfile`, `nginx.conf`, `security-headers.conf`, `public/theme.js`) | public | `web/README.md` and `web/CLAUDE.md`: rewrite (mention the cluster, VPN and `MAIAK_API_TARGET` on `klr-ops.net`) |
| `web/src/ee/` | ee | Placeholder, created with the first paid web feature |
| `deploy/helm/maiak/templates/**`, `Chart.yaml`, `values.schema.json`, `.helmignore` | public, Apache-2.0 | Chart name `maiak` (section 3) |
| `deploy/helm/maiak/values.yaml` | rewrite | Defaults name `maiak.klr-ops.net`, `envoy-gateway-internal`/`envoy-gateway`, `platform/grafana/db`, `platform/maiak/*`, `klr-platform` in comments; replace with neutral defaults and examples |
| `deploy/helm/maiak/README.md` | rewrite | Mentions `klr-platform`, `klr-ops.net`, Grafana's RDS, `eu-central-1`, `platform/*` secret paths |
| `deploy/helm/maiak/templates/networkpolicy.yaml` | rewrite | Comment names `klr-platform` |
| `deploy/helm/maiak/values-platform.yaml` | private | **Do not move before the split**: ArgoCD (`argocd-application.yaml`, `devops-charts-infra`) and `release.yml` reference this path. Holds the RDS endpoint `grafana.czqsaiesae4n.eu-central-1.rds.amazonaws.com`, hostnames, gateway names |
| `deploy/helm/maiak/argocd-application.yaml` | private | ArgoCD Application of `klr-platform`, repo URL, `devops-charts-infra` |
| `deploy/db/bootstrap.sql` | public | Manual fallback of the bootstrap; check comments for internal names before publishing |
| `.github/workflows/ci.yml`, `.github/actionlint.yaml`, `.github/spectral.yaml`, `.github/dependabot.yml` | public | |
| `.github/workflows/release.yml` | rewrite + private | Image build and push to a public registry stays public (two images, community and enterprise, ADR-0105). The tag bump commit into `values-platform.yaml` moves to the private repository |
| `scripts/check-todos.sh`, `scripts/validate-chart.sh`, `scripts/check-rendered-chart.py` | public | `validate-chart.sh` renders `values-platform.yaml`: give the public copy an example values file instead |
| `scripts/bump-image-tag.sh` | private | Edits `values-platform.yaml` |
| `CLAUDE.md` | rewrite | The "Deployment" section (`klr-platform`, `ghcr.io/theechofive/maiak-*`, `maiak.klr-ops.net`, `platform/maiak/...`, `--profile`) goes to the private repository's `CLAUDE.md` |
| `README.md` | rewrite | Check for internal names; add licence, trademark policy and edition notes |
| `docs/adr/**` | public, except below | ADRs are design history; scrub internal names where they appear |
| `docs/adr/0010-deployment.md`, `0014-db-bootstrap-automation.md`, `0091-generated-admin-and-encryption-secrets.md` | rewrite (or private) | Name `klr-platform`, `devops-charts-infra`, Grafana's RDS, `platform/*` secrets |
| `docs/adr/0015`, `0092`, `0101`, `0102`, `0103`, `0104` | rewrite | Mention `klr-platform`, `klr.com.ua` or the VPN in passing |
| `docs/architecture/**`, `docs/design-system.md` | public | `overview.md` and `data-model.md` mention `klr-ops.net` / `eu-central-1`: rewrite |
| `docs/runbooks/migrations.md`, `invitations.md`, `admin-panel.md`, `status-page.md`, `oauth.md` | rewrite | Product runbooks for any operator; replace `klr-platform`, `klr-ops.net`, `klr.com.ua`, `platform/maiak/*` with placeholders. Keep a private copy with the real values |
| `docs/runbooks/first-deploy.md`, `first-install.md`, `database-bootstrap.md`, `heartbeat-alertmanager.md`, `observability.md` | private | Operations of our installation (RDS shared with Grafana, VictoriaMetrics, ArgoCD, AWS profiles). A generic self-hosted install guide is written new for the public repository |
| `docs/LEDGER.md` | rewrite + private | Split: product entries public, operations entries (R-001, R-009, L-002, L-009, L-100, L-102, R-010, deployment risks) private |
| `docs/PROJECT.md` | private | Internal brief (cluster, RDS, ownership) |
| `docs/tasks/**` (incl. this file) | private | Internal planning history; not useful publicly |
| `.dockerignore`, `.gitignore` | public | |
| `.omc/`, local scratch | delete | Not tracked; make sure it stays out |
| Git history | decide at the split | The public repository starts from a squashed import (history contains every internal name above and earlier values files); the private repository keeps the full history |

## 2. Internal information found (grep)

| What | Pattern | Files (tracked) | Action |
|---|---|---|---|
| Internal hostname | `klr-ops.net` | 19, incl. `api/openapi.yaml`, generated Go and TS clients, `config_test.go`, chart values, docs | Placeholder `maiak.example.com` (after the rename: the new name) |
| Cluster name | `klr-platform` | 22, incl. a comment in `backend/migrations/20261020120000_monitors_locations.sql` (immutable: leave or accept) and the chart's `networkpolicy.yaml` | Remove from public files; the migration comment is harmless but visible |
| Company domain | `klr.com.ua` | 4 docs (ADR-0102, `first-install.md`, `oauth.md`, `phase4.md`) | Placeholder `example.com` |
| Personal GitHub account | `theechofive` | 139 (Go module path, imports, image names, spec contact, `release.yml`) | Changes with the rename and the move to an organization account |
| AWS region | `eu-central-1` | 24 (default worker location, seed migration, tests, docs) | Product default `home`; keep only in the private values |
| RDS endpoint | `czqsaiesae4n` / `rds.amazonaws.com` | `values-platform.yaml` only | Private |
| Gateway names | `envoy-gateway`, `envoy-gateway-internal` | `values.yaml`, `values-platform.yaml`, `first-deploy.md` | Neutral defaults in public values |
| Infra repository | `devops-charts-infra` | 6 (Application manifest, docs) | Private |
| Grafana's database and secret | `grafana`, `platform/grafana/db` | 11 + code comments in `dbbootstrap`, `secretsbootstrap` | Public code comments say "the instance admin"; specifics private |
| Secret paths | `platform/maiak/*` | 15 | Chart default stays configurable (`remoteKey`); examples become placeholders |
| AWS CLI profiles | `--profile` | 5 runbooks (placeholders only, no real profile names) | Private runbooks |
| Monitoring stack | `VictoriaMetrics`, `vmalert` | `LEDGER.md`, `observability.md` | Private |
| Secret-looking strings | `BEGIN … PRIVATE KEY`, `AKIA…`, `ghp_…`, `xoxb-`/`xoxp-` | none | — |
| Slack webhook URLs | `hooks.slack.com/services/T…` | 11 test files | All fake (`T0/B0/x`, `T000/B000/XXXX`, …); keep |
| AWS account ids / ARNs | `arn:aws` | none (12-digit matches are UUIDs and timestamps) | — |

## 3. Where the name `maiak` appears, by cost of change

About 2 600 occurrences in 300 tracked files (case-insensitive). Grouped so the rename is mechanical: do group A anywhere, group B in one commit with regeneration, group C only together with a reinstall or a planned migration.

**A. Text only (cheap, no runtime effect).**
- Prose in docs, ADRs, runbooks, comments, test names; UI copy and the page title (`Maiak` in `web/src`, `document.title`, login screen) and the public status page footer "Powered by Maiak" (`backend/internal/statuspages/public/assets/templates.html`); `web/package.json` name `maiak-web`; `info.title` of the spec.

**B. Identifiers in code and build (medium: one coordinated commit, regenerate, redeploy).**
- Go module path `github.com/theechofive/maiak/backend` (537 hits, 129 files) and the binary `maiakctl` (58 hits, 26 files; image entrypoints `/app/maiakctl` in the chart).
- Images `ghcr.io/theechofive/maiak-backend`, `maiak-web` (11 hits, 7 files: `release.yml`, chart values, docs) and the ghcr packages themselves.
- Environment variables `MAIAK_*` (458 hits, 73 files: `internal/platform/config`, chart `_helpers.tpl`, docs, tests). Changing them changes every deployment's configuration; a transition period that reads both prefixes avoids a flag day.
- Prometheus metric names prefixed `maiak_` (`maiak_build_info`, `maiak_http_requests_total`, every service's own metrics; about 160 hits): dashboards and alert rules (private) break; rename together.
- Problem type URNs `urn:maiak:problem:*` (241 hits, 31 files): part of the API contract; the web app and any client match on them. Breaking change (ADR needed) unless both are accepted for a release.
- Browser storage keys `maiak-theme`, `maiak-project:<user>:<tenant>`: users lose a preference; harmless.
- Chart name `maiak`, release name and resource names (`maiak-api`, `maiak-db-bootstrap`, ...), Kubernetes labels, ServiceMonitor names; `ApplicationName` `maiak-api` etc. in `pg_stat_activity`.
- Advisory lock names `hashtext('maiak.*')` inside definer functions and `dbbootstrap.lockKey` (`0x6d6169616b627331`, "maiakbs1"): change only through a new migration (`CREATE OR REPLACE`), all at once.

**C. Persisted or operational state (expensive: needs the reinstall, or careful migration and secret moves).**
- Database name `maiak` and roles `maiak_owner`, `maiak_api`, `maiak_worker`, `maiak_statuspage` (577 hits, 97 files). Every migration grants to them by name, and applied migrations are immutable: renaming means either `ALTER ROLE ... RENAME` plus editing historical migration text (acceptable only before the public release, with a fresh install), or keeping the role names.
- Kubernetes Secrets `maiak-db`, `maiak-admin`, `maiak-admin-initial`, `maiak-crypto`, `maiak-statuspage`, `maiak-oauth` (183 hits, 36 files): created at runtime, not by Helm; renaming means recreating them (the encryption key in `maiak-crypto` must be carried over or every stored webhook is lost).
- AWS Secrets Manager paths `platform/maiak/*`: new secrets, then switch `remoteKey`.
- Cookies `maiak_session`, `__Host-maiak_oauth` (27 hits, 7 files): renaming signs everyone out once and cancels OAuth flows in progress.
- Hostname `maiak.klr-ops.net`, DNS record, OAuth redirect URIs registered at Google and GitHub (they include the host).
- ArgoCD Application `maiak`, namespace `maiak`, `devops-charts-infra` entries.
- Seeded data: location code `home-eu-central-1`, plan names, the `default` organization: not the project name, but part of the same clean-up.
