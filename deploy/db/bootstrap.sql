-- Bootstrap of the Maiak database and roles inside an existing RDS PostgreSQL 15 instance.
-- Run ONCE by an administrator as the RDS master user, connected to the `postgres` database.
-- Passwords are NOT in this file: the runbook streams `\set owner_pw '...'`, `\set api_pw '...'`
-- and `\set worker_pw '...'` lines to psql before this script (see docs/runbooks/database-bootstrap.md).
-- Idempotent where practical; fails fast on any error.

\set ON_ERROR_STOP on

-- Roles. None is superuser, none can bypass RLS, only the owner can log in to run migrations.
-- Connection limits matter because the instance is shared with other tools (ledger R-001).
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'maiak_owner') THEN
    CREATE ROLE maiak_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 3;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'maiak_api') THEN
    CREATE ROLE maiak_api LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 20;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'maiak_worker') THEN
    CREATE ROLE maiak_worker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 20;
  END IF;
END
$$;

ALTER ROLE maiak_owner  PASSWORD :'owner_pw';
ALTER ROLE maiak_api    PASSWORD :'api_pw';
ALTER ROLE maiak_worker PASSWORD :'worker_pw';

-- Safety timeouts per role.
ALTER ROLE maiak_api    SET statement_timeout = '15s';
ALTER ROLE maiak_api    SET idle_in_transaction_session_timeout = '30s';
ALTER ROLE maiak_worker SET statement_timeout = '30s';
ALTER ROLE maiak_worker SET idle_in_transaction_session_timeout = '30s';
ALTER ROLE maiak_owner  SET statement_timeout = '5min';

-- On RDS the creating admin must be a member of the future owner role to create a database owned by it.
GRANT maiak_owner TO CURRENT_USER;

SELECT 'CREATE DATABASE maiak OWNER maiak_owner ENCODING ''UTF8'''
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'maiak')
\gexec

REVOKE ALL ON DATABASE maiak FROM PUBLIC;
GRANT CONNECT ON DATABASE maiak TO maiak_owner, maiak_api, maiak_worker;

\connect maiak

-- citext is a trusted extension in PostgreSQL 13+; created here so migrations do not need extra privileges.
CREATE EXTENSION IF NOT EXISTS citext;

ALTER SCHEMA public OWNER TO maiak_owner;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO maiak_api, maiak_worker;

-- Drop the temporary membership so the master user does not keep the owner role.
REVOKE maiak_owner FROM CURRENT_USER;

-- Verification (printed for the operator).
SELECT rolname, rolsuper, rolbypassrls, rolconnlimit FROM pg_roles WHERE rolname LIKE 'maiak\_%' ORDER BY rolname;
SELECT datname, pg_get_userbyid(datdba) AS owner FROM pg_database WHERE datname = 'maiak';
