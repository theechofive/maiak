// Package dbbootstrap implements `maiakctl db bootstrap`: it creates (or repairs) the Maiak
// database and its four roles inside an existing PostgreSQL 15 instance and keeps their
// passwords in a Kubernetes Secret. It runs in a hook Job before the migration Job and is
// idempotent: running it again changes nothing except re-applying the same passwords.
//
// # Kubernetes Secret contract
//
// The Secret (default name maiak-db, in the pod's namespace) is type Opaque with exactly
// these keys:
//
//	owner_password   password of maiak_owner (migrations)
//	api_password     password of maiak_api (cmd/api)
//	worker_password      password of maiak_worker (cmd/worker, cmd/jobs)
//	statuspage_password  password of maiak_statuspage (cmd/statuspage, ADR-0103)
//	owner_url            postgres://maiak_owner:<pw>@<host>:<port>/<db>?sslmode=<mode>
//	api_url              postgres://maiak_api:<pw>@<host>:<port>/<db>?sslmode=<mode>
//	worker_url           postgres://maiak_worker:<pw>@<host>:<port>/<db>?sslmode=<mode>
//	statuspage_url       postgres://maiak_statuspage:<pw>@<host>:<port>/<db>?sslmode=<mode>
//
// Passwords are 32 random bytes, URL-safe base64 without padding (43 characters); in the URLs
// they are percent-escaped anyway. The workloads read only the *_url keys.
//
// Behaviour:
//   - Secret exists with the owner, api and worker *_password keys: those passwords are used (so a manual
//     rotation is done by editing the Secret and re-running). Missing or stale *_url keys (for
//     example after a host or password change) are rebuilt with a merge patch and a drift
//     notice naming only the keys is logged; the password keys are never rewritten.
//   - Secret exists without statuspage_password (written before P-05): that password and its URL
//     are generated and added with a merge patch before any role is changed.
//   - Secret does not exist: four passwords are generated and the Secret is created BEFORE
//     any role is changed, so a password is never set in the database without being stored.
//     If another run created it in the meantime, that Secret is read and used.
//   - Secret exists but lacks a password key: the run fails; it never overwrites a Secret it
//     did not write completely.
//
// The Job's ServiceAccount needs get, create and patch on secrets in its namespace (the token, CA
// bundle and namespace are read from /var/run/secrets/kubernetes.io/serviceaccount).
//
// # Database changes
//
// Connected to database "postgres" as the instance admin (on RDS the master user, a member of
// rds_superuser and not a real superuser), under a cluster-wide advisory lock:
//   - roles maiak_owner (CONNECTION LIMIT 3), maiak_api (20), maiak_worker (20),
//     maiak_statuspage (10): LOGIN,
//     NOSUPERUSER, NOCREATEDB, NOCREATEROLE, NOREPLICATION, NOBYPASSRLS. Superuser,
//     replication and bypassrls can only be changed by a real superuser, so they are set at
//     creation and verified (not altered) afterwards; the run fails if one is set.
//   - passwords are sent as SCRAM-SHA-256 verifiers computed here, so the plaintext never
//     reaches the server (or its statement log);
//   - role settings: statement_timeout (owner 5min, api 15s, worker 30s, statuspage 5s),
//     idle_in_transaction_session_timeout (30s api and worker, 10s statuspage) and
//     default_transaction_read_only for maiak_statuspage;
//   - database OWNER maiak_owner (the admin is temporarily granted maiak_owner, as RDS
//     requires, and the membership is revoked at the end), REVOKE ALL FROM PUBLIC, CONNECT for
//     the four roles;
//   - inside the database: schema public owned by maiak_owner, no privileges for PUBLIC, USAGE
//     for maiak_api, maiak_worker and maiak_statuspage, extension citext created as maiak_owner
//     (so no object in the database is owned by the admin login), and EXECUTE on
//     status_page_snapshot(text) for maiak_statuspage once the migration created it (the
//     migration grants it too when the role already exists).
//
// Verification fails if a role has an unsafe attribute, the database has another owner, or
// a runtime role (maiak_api, maiak_worker, maiak_statuspage) is a member of maiak_owner. The summary also reports whether the
// admin is still a member of maiak_owner after the temporary grant is revoked.
//
// Passwords and URLs are never logged. The summary printed at the end lists role attributes
// only.
package dbbootstrap
