# Runbook: bootstrap the Maiak database in RDS

Creates database `maiak` and roles `maiak_owner`, `maiak_api`, `maiak_worker` inside the existing RDS instance `grafana` (account of `klr-platform`, region `eu-central-1`; ADR-0010, risk R-001). Run once, by a human or an agent acting with the human's approval.

Safety rules: explicit `--context` for kubectl and `--profile klr-platform` for AWS CLI, never the default context. Passwords never appear in command arguments, pod specs, files in Git, logs or chat. They are streamed over stdin.

## 1. Find the master credentials (read-only)
`aws rds describe-db-instances --profile klr-platform --region eu-central-1 --db-instance-identifier grafana` shows endpoint, master username and whether the master secret is RDS-managed (`MasterUserSecret`). Fetch the password from Secrets Manager into a shell variable only; never echo it.

## 2. Generate three random passwords (32+ chars, URL-safe) locally
Keep them in shell variables for this session only.

## 3. Start a throw-away psql pod
Namespace `maiak-bootstrap`, image `postgres:15-alpine` (multi-arch, arm64 works), no ServiceAccount token, resource limits small, `sleep` as the command. The pod must be in a namespace/VPC path that can reach the RDS endpoint (the cluster nodes are in the same VPC). Delete the pod and the namespace at the end (step 6).

## 4. Run the script
Stream, via `kubectl exec -i`, a stdin made of: `\set owner_pw '...'`, `\set api_pw '...'`, `\set worker_pw '...'`, then the contents of `deploy/db/bootstrap.sql`. The master password is provided to `psql` through a `PGPASSFILE` streamed to the pod the same way. Connect to database `postgres` with `sslmode=require`.

## 5. Verify
The script prints the role attributes and the database owner. Expect: three `maiak_*` roles, `rolsuper=f`, `rolbypassrls=f`, database `maiak` owned by `maiak_owner`. Then log in once as each role (`SELECT current_user`) and confirm that `maiak_api` cannot create tables.

## 6. Store credentials, clean up
Create the secret `platform/maiak/db` in AWS Secrets Manager (profile `klr-platform`) as JSON with `api_url`, `worker_url`, `owner_url` (`postgres://<role>:<password>@<endpoint>:5432/maiak?sslmode=require`). Create `platform/maiak/crypto` (`keys` = JSON `{"k1":"<base64 32 random bytes>"}`, `active_key_id` = `k1`). The admin secret `platform/maiak/admin` (`email`, `password_hash`) is created by the owner: generate the hash with `maiakctl hashpw`. Delete the pod and namespace `maiak-bootstrap`. Unset the shell variables.

## Notes
- `sslmode=require` does not verify the server certificate. Switching to `verify-full` with the RDS CA bundle is tracked in L-018.
- The Maiak roles can technically connect to other databases on the instance because `PUBLIC` has `CONNECT` by default, but they own and are granted nothing there. Do not change other tools' databases from this runbook.
- Rotation: change a role password with `ALTER ROLE ... PASSWORD`, update the secret; ExternalSecret has `refreshInterval: 0`, so recreate the Secret and restart the workloads.
