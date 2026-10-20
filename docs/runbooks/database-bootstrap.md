# Runbook: database bootstrap

The whole first deployment, including this step in context, is in [first-deploy.md](first-deploy.md).

Decision and rationale: [ADR-0014](../adr/0014-db-bootstrap-automation.md). The database `maiak` and the roles `maiak_owner`, `maiak_api`, `maiak_worker` are created automatically by the hook Job `maiak-db-bootstrap` before the migration Job. This page covers enabling it, verifying it and the manual fallback.

## First deployment on klr-platform
1. In `deploy/helm/maiak/values-platform.yaml` the flag `dbBootstrap.enabled` is `true` (host: the RDS endpoint of `grafana`). Prerequisite secrets in AWS Secrets Manager (account of `klr-platform`): `platform/maiak/crypto` (`keys`, `active_key_id`) and `platform/maiak/admin` (`email`, `password_hash` from `maiakctl hashpw`). `platform/grafana/db` already exists.
2. Sync the ArgoCD Application. Order: admin ExternalSecret and RBAC (wave -4), bootstrap Job (-3), other ExternalSecrets (-2), migrate Job (-1), workloads (0).
3. Verify: `kubectl --context <klr-platform context> -n maiak logs job/maiak-db-bootstrap` shows the role attributes summary (no secrets): three `maiak_*` roles with `rolsuper=f`, `rolbypassrls=f`, database `maiak` owned by `maiak_owner`. Confirm the Secret `maiak-db` exists (keys only, never print values) and that the temporary Secret `maiak-db-admin` was removed after the Job.
4. Set `dbBootstrap.enabled: false` in `values-platform.yaml` and commit. The admin login then no longer appears in the namespace (risk R-009).

## When to enable it again
A new database or environment, a dedicated RDS (L-009), or regenerating role passwords (delete the Secret `maiak-db`, enable the flag, sync; the Job regenerates and runs `ALTER ROLE`; restart the workloads to pick up the new Secret).

## Manual fallback
Run the same command by hand against an instance you can reach, with the same environment variables the Job uses (`MAIAK_BOOTSTRAP_HOST`, `MAIAK_BOOTSTRAP_PORT`, `MAIAK_BOOTSTRAP_ADMIN_USER`, `MAIAK_BOOTSTRAP_ADMIN_PASSWORD`, `MAIAK_BOOTSTRAP_DB_NAME`, `MAIAK_BOOTSTRAP_SSLMODE`, `MAIAK_BOOTSTRAP_SECRET_NAME`, `MAIAK_BOOTSTRAP_NAMESPACE`) from a pod or machine with access to the Kubernetes API for the Secret. Passwords must never be passed as command arguments or committed.

## Notes
- `sslmode=require` does not verify the server certificate; switching to `verify-full` with the RDS CA bundle is tracked in L-018.
- Maiak roles can connect to other databases of the instance (default `PUBLIC` connect) but own and are granted nothing there.
