-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Default privileges for every object maiak_owner creates from now on. Runtime roles get DML on
-- tables (RLS still filters tenant tables) and sequence usage; tables that need less revoke it
-- explicitly below. Functions get no EXECUTE by default: SECURITY DEFINER functions are granted
-- one by one. The function default is global (not IN SCHEMA) because a per-schema default can
-- only add to the global one, and the global one grants EXECUTE to PUBLIC.
ALTER DEFAULT PRIVILEGES FOR ROLE maiak_owner IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO maiak_api, maiak_worker;
ALTER DEFAULT PRIVILEGES FOR ROLE maiak_owner IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO maiak_api, maiak_worker;
ALTER DEFAULT PRIVILEGES FOR ROLE maiak_owner
    REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

-- Tenants are the root of tenancy (no tenant_id column), so they are not under RLS.
CREATE TABLE tenants (
    id          uuid        PRIMARY KEY,
    slug        text        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    status      text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    limits      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);
-- Soft delete only; the api upserts the default tenant at startup.
REVOKE DELETE ON tenants FROM maiak_api, maiak_worker;
REVOKE INSERT, UPDATE ON tenants FROM maiak_worker;

-- Users are global (one person can belong to several tenants).
CREATE TABLE users (
    id             uuid        PRIMARY KEY,
    email          citext      NOT NULL UNIQUE CHECK (length(email) BETWEEN 3 AND 320),
    password_hash  text,
    display_name   text        NOT NULL DEFAULT '' CHECK (length(display_name) <= 200),
    status         text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
-- users has no RLS, so access is narrowed by grants: the worker needs nothing, the api may
-- not delete users or rewrite id/created_at.
REVOKE ALL ON users FROM maiak_worker;
REVOKE UPDATE, DELETE ON users FROM maiak_api;
GRANT UPDATE (email, password_hash, display_name, status, updated_at) ON users TO maiak_api;

CREATE TABLE memberships (
    tenant_id   uuid        NOT NULL REFERENCES tenants (id),
    user_id     uuid        NOT NULL REFERENCES users (id),
    role        text        NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX memberships_user_id_idx ON memberships (user_id);
REVOKE ALL ON memberships FROM maiak_worker;

-- For future OAuth providers; unused in slice 1.
CREATE TABLE auth_identities (
    user_id     uuid        NOT NULL REFERENCES users (id),
    provider    text        NOT NULL CHECK (length(provider) BETWEEN 1 AND 64),
    subject     text        NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject)
);
CREATE INDEX auth_identities_user_id_idx ON auth_identities (user_id);
REVOKE ALL ON auth_identities FROM maiak_worker;
REVOKE UPDATE ON auth_identities FROM maiak_api;

-- Only the SHA-256 of the session token is stored. A session belongs to one membership.
CREATE TABLE sessions (
    id            uuid        PRIMARY KEY,
    user_id       uuid        NOT NULL,
    tenant_id     uuid        NOT NULL,
    token_hash    bytea       NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    csrf_token    text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    ip            inet,
    user_agent    text        CHECK (length(user_agent) <= 512),
    FOREIGN KEY (tenant_id, user_id) REFERENCES memberships (tenant_id, user_id) ON DELETE CASCADE
);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_tenant_user_idx ON sessions (tenant_id, user_id);
REVOKE ALL ON sessions FROM maiak_worker;

CREATE TABLE audit_log (
    id             uuid        PRIMARY KEY,
    tenant_id      uuid        NOT NULL REFERENCES tenants (id),
    actor_user_id  uuid        REFERENCES users (id),
    action         text        NOT NULL CHECK (length(action) BETWEEN 1 AND 100),
    entity_type    text        NOT NULL CHECK (length(entity_type) BETWEEN 1 AND 100),
    entity_id      text,
    metadata       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    ip             inet,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_tenant_created_idx ON audit_log (tenant_id, created_at DESC);
-- Insert-only for runtime roles.
REVOKE SELECT, UPDATE, DELETE ON audit_log FROM maiak_api, maiak_worker;

-- Row-level security on every table with tenant_id. FORCE makes it apply to the owner too.
-- NULLIF: after a SET LOCAL in an earlier transaction the setting reads as '' (not NULL) on a
-- pooled connection; without NULLIF a query outside WithTenantTx would fail with a cast error on
-- some connections and return nothing on others. With it, it always sees no rows.
ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON memberships
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sessions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
-- auth_lookup_session (below) runs as maiak_owner and must find a session before the tenant is
-- known. Policies are permissive (OR-ed), so this read-only policy is limited to the owner role;
-- runtime roles still see only their tenant.
CREATE POLICY owner_session_lookup ON sessions FOR SELECT TO maiak_owner USING (true);

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_log
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Session lookup by token hash. The tenant is not known until the session is found, so this
-- cannot run under the tenant policy. SECURITY DEFINER makes it run as maiak_owner (allowed by
-- owner_session_lookup); search_path is pinned so a caller cannot shadow objects; it returns
-- only a live session for an exact 32-byte hash, so it cannot be used to enumerate sessions.
-- +goose StatementBegin
CREATE FUNCTION auth_lookup_session(p_token_hash bytea)
RETURNS TABLE (
    id            uuid,
    user_id       uuid,
    tenant_id     uuid,
    csrf_token    text,
    created_at    timestamptz,
    expires_at    timestamptz,
    last_seen_at  timestamptz
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT s.id, s.user_id, s.tenant_id, s.csrf_token, s.created_at, s.expires_at, s.last_seen_at
    FROM sessions AS s
    WHERE s.token_hash = p_token_hash
      AND s.expires_at > now()
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION auth_lookup_session(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_lookup_session(bytea) TO maiak_api;
