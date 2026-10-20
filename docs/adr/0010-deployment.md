# ADR-0010: Deployment to klr-platform with our own chart, ArgoCD and ghcr

- Status: Accepted
- Date: 2026-10-20

## Context
`klr-platform` (EKS, arm64 nodes, single AZ, memory-tight) is the only environment for development and MVP. Findings: Envoy Gateway API (no Ingress objects), only an internal gateway with wildcard `*.klr-ops.net`, cert-manager with a Cloudflare DNS-01 issuer, External Secrets with `ClusterSecretStore aws-secrets`, ArgoCD fed from the `devops-charts-infra` repo, NetworkPolicy not enforced, victoria-metrics stack present. The platform's RDS instances are one per tool (`grafana`, `glitchtip`).

## Decision
- Own Helm chart in `deploy/helm/maiak` (not the company `helm-app`), one ArgoCD Application registered in `devops-charts-infra`, namespace `maiak`.
- Host `maiak.klr-ops.net` on the internal gateway: `/api` to the api, `/` to the web. The CNAME to the internal NLB is added manually in Cloudflare.
- Images: multi-arch (`amd64` + `arm64`) at `ghcr.io/theechofive/maiak-backend` and later `maiak-web`. CI tags with the short SHA, pushes, and commits the tag into `values-platform.yaml`; ArgoCD syncs. No image-updater dependency.
- Database: new database `maiak` with roles `maiak_owner`, `maiak_api`, `maiak_worker` inside the existing RDS instance `grafana` (accepted risk R-001, to be moved: L-009). Bootstrap via `docs/runbooks/database-bootstrap.md`.
- Secrets via ExternalSecret from AWS Secrets Manager `platform/maiak/{db,admin,crypto}`.
- Migrations run as an ArgoCD PreSync Job with the owner role.
- Small resource requests; no hostNetwork on the worker; container hardening (non-root, read-only root filesystem, drop ALL, seccomp RuntimeDefault).

## Consequences
- Open decision L-002 (ghcr pull secret) blocks the first deploy.
- Two stateful systems share an RDS instance until L-009.
- NetworkPolicy templates ship disabled (R-002).

## Alternatives considered
- CloudNativePG in the cluster: new operator in a shared cluster and tight memory.
- A new dedicated RDS now: cleaner, deferred to L-009 to keep the MVP cheap.
- Reusing `helm-app`: its values schema is not visible and it couples the product to company internals.
