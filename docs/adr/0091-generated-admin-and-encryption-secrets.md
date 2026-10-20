# ADR-0091: Admin login and encryption key generated in the cluster by default

- Status: Accepted
- Date: 2026-10-20
- Amends: [ADR-0009](0009-auth-admin-users.md) (where the admin Secret comes from), [ADR-0014](0014-db-bootstrap-automation.md) (encryption keys no longer have to live in AWS Secrets Manager)

## Context
The chart required two ExternalSecrets, `platform/maiak/admin` (`email`, `password_hash`; `login` since B-24) and `platform/maiak/crypto` (`keys`, `active_key_id`). Until the owner created both by hand in AWS Secrets Manager, the first sync failed. That is a manual, error-prone step (JSON-in-JSON for the keys, an argon2id hash made on a laptop) for a single-admin internal tool.

## Decision
- **Per-Secret source, generate by default.** `secrets.admin.source` and `secrets.crypto.source` are `generate` (default) or `externalSecret`. In `externalSecret` mode the chart renders the ExternalSecret as before (`secrets.<name>.remoteKey`, formerly `externalSecrets.<name>.remoteKey`). In `generate` mode the hook Job `maiak-secrets-bootstrap` runs `maiakctl secrets bootstrap` and creates the Secret if it does not exist. Workloads are unchanged: they read `maiak-admin` and `maiak-crypto` with the same keys.
- **Never overwrite.** A complete existing Secret is left untouched. An existing but incomplete one fails the Job with a message naming the missing keys: guessing could lock the owner out or orphan encrypted data. Creates are create-if-absent; a 409 from a concurrent run is resolved by re-reading what the winner stored.
- **Admin is a system user `admin`** (like Grafana; `secrets.admin.login`, 1-64 of `[a-z0-9._@-]`). The Job generates 24 random bytes (base64url, 32 characters) and stores:
  1. first the **initial Secret** `maiak-admin-initial` with `login` and `password` in plaintext, annotated "read once, then delete" (the `argocd-initial-admin-secret` pattern);
  2. then `maiak-admin` with `login` (key renamed from `email` by B-24) and `password_hash`, an argon2id PHC string made with the same function and defaults as `maiakctl hashpw`.
  The order guarantees that the readable password always matches the hash. If the initial Secret exists without `maiak-admin` (a run stopped in between), the next run hashes the stored password. If `maiak-admin` exists without the initial Secret, the owner deleted it after logging in: nothing is recreated.
- **Encryption key**: 32 bytes from `crypto/rand`, `keys` = `{"<keyId>":"<std base64>"}`, `active_key_id` = keyId (`secrets.crypto.keyId`, default `k1`), the formats `config.LoadAPI` parses.
- **Separate Job without database credentials.** The secrets Job has its own ServiceAccount (token mounted) and a Role limited to `create` on secrets and `get,update,patch` on exactly `maiak-admin`, `maiak-admin-initial`, `maiak-crypto`. It never sees the RDS master login or `maiak-db`. It is rendered whenever a source is `generate` and stays enabled permanently: unlike `dbBootstrap` (R-009) it holds nothing dangerous and is a no-op once the Secrets exist. Waves: RBAC -4, Job -3, like the db bootstrap.
- **Logging**: only Secret names and key names, never the password, hash or key (tested by capturing the log).
- **Password reset**: delete `maiak-admin` (and `maiak-admin-initial` if present) and re-sync; a new password appears in a new initial Secret. The api deletes the admin's sessions when the hash changes (ADR-0009). An in-app password change is future work (L-150).

## Why losing a generated key is acceptable
The encryption key only protects the tenant's Slack webhook URL (ADR-0050; `notify` is its only user). If the namespace or the Secret is lost, the webhook becomes unreadable and is re-entered in Settings; monitors, incidents and results are not encrypted. That trade-off removes the main reason (ADR-0014) to keep the key outside the cluster. Whoever needs a durable key uses `externalSecret` mode.

## Consequences
- The first sync needs no AWS Secrets Manager entry for Maiak itself (only `platform/grafana/db` for the db bootstrap).
- The initial password is readable by anyone with `get` on Secrets in namespace `maiak` until the owner deletes `maiak-admin-initial` (R-110).
- A generated key lives only in the cluster and dies with the namespace (or with deleting `maiak-crypto`); rotation is manual (L-151).
- Note (B-24, 2026-10-20): the api now reads `MAIAK_ADMIN_LOGIN` and accepts the default `admin`; the Secret key is `login` in both modes, and the logins are case-insensitive (ADR-0009 amendment). The former workaround of an email-shaped login is no longer needed (L-152 closed).
- Switching a Secret from `externalSecret` to `generate`: ArgoCD prunes the ExternalSecret and, with `creationPolicy: Owner`, ESO's Secret goes with it after the Job already saw it; the next sync generates a new one (new password, new key). Switching the other way: delete the generated Secret in the same change window, so ESO creates it from Secrets Manager instead of meeting a Secret it does not own (how ESO treats that case was not verified).

## Alternatives considered
- Keep Secrets Manager mandatory: safest for the key, but blocks the first deploy on a manual step for little value.
- Helm `lookup` + `randAlphaNum` in templates: ArgoCD renders without cluster access, so `lookup` returns nothing and every sync would regenerate the Secret.
- Generate inside the db bootstrap Job: that Job holds the RDS master login and is meant to be switched off; mixing the two would keep it on.
- Print the password in the Job log: logs are shipped and retained; a Secret can be deleted.
