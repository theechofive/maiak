# Deploy tasks for the MVP (klr-platform)

Design: [ADR-0010](../adr/0010-deployment.md), [ADR-0014](../adr/0014-db-bootstrap-automation.md). Chart: `deploy/helm/maiak`.

| ID | Task | Depends on | Status |
|---|---|---|---|
| D-01 | Chart baseline: api, worker, jobs, optional web, HTTPRoute on the internal gateway, ExternalSecrets, migration hook, DB bootstrap hook, sync waves, schema, hardening | none | done |
| D-02 | CI: lint, tests against Postgres, helm lint, repo hygiene, migration naming, paths-filter fix for push. Written and linted with actionlint; stays red until the Makefile, Dockerfile and golangci config exist | B-06 | blocked |
| D-03 | Chart completion for the finished MVP: `web.enabled=true` in `values-platform.yaml`, web image repository and tag key CI can rewrite, new env (`MAIAK_PUBLIC_URL`, `MAIAK_BLOCKED_CIDRS`), encryption keys only for api and jobs (not the worker), probes for all workloads, resource review against the memory budget, `ServiceMonitor` template kept disabled (L-003) | F-03 to F-08, F-10 | todo |
| D-04 | Release pipeline: backend image on every change, web image after F-10 (same short-SHA tag format as the backend), both tags written to `values-platform.yaml`; verify the bump logic on a copy | F-10, B-06 | todo |
| D-05 | First-deploy checklist and Application manifest example for `devops-charts-infra` (`docs/runbooks/first-deploy.md`): prerequisites (secrets, DNS CNAME, pull secret decision L-002), order of steps, verification, switching `dbBootstrap` off | D-03 | todo |
| D-06 | Static validation of the rendered chart: `helm lint`, `helm template` for all value combinations, `kubeconform` (or equivalent) against the Gateway API, External Secrets and core schemas if available offline | D-03 | todo |

## Out of scope for the MVP
Actual deployment to the cluster (done by ArgoCD after the owner merges the Application and decides L-002), NetworkPolicy enforcement, ServiceMonitor, dedicated RDS.
