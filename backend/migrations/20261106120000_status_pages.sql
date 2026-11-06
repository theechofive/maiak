-- +goose Up
SET LOCAL lock_timeout = '5s';

-- P-05 (ADR-0103): public status page. Additive only: the previous release never reads these
-- tables, the new function or the new plan key.
--
-- status_pages            the configuration of a page (one per organization by plan, L-270)
-- status_page_components  monitors and heartbeats of any project shown under a public name
-- status_page_snapshots   the prepared public view, built by the statuspages module (api on a
--                         configuration change, jobs every 30 s); the only thing the public
--                         statuspage process can read, through status_page_snapshot(slug)

LOCK TABLE plans IN ROW EXCLUSIVE MODE;

CREATE TABLE status_pages (
    id            uuid        PRIMARY KEY,
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    -- Global and public: a slug is not a secret. Reserved words are refused here too, so a
    -- route of the gateway or the renderer can never be shadowed by a page (ADR-0103).
    slug          text        NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{2,62}$' AND slug NOT IN (
                      'admin', 'api', 'app', 'assets', 'auth', 'badge', 'dashboard', 'docs', 'embed',
                      'feed', 'health', 'healthz', 'help', 'login', 'logout', 'maiak', 'metrics',
                      'public', 'readyz', 'register', 'robots', 'root', 'rss', 'settings', 'signup',
                      'static', 'status', 'support', 'system', 'www')),
    title         text        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 80),
    description   text        NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    -- Shown with <img>, never fetched by the server.
    logo_url      text        CHECK (logo_url IS NULL OR (char_length(logo_url) <= 500 AND logo_url ~ '^https://[^\s/?#@]+')),
    accent_color  text        CHECK (accent_color IS NULL OR accent_color ~ '^#[0-9a-f]{6}$'),
    -- argon2id PHC string; NULL = no password.
    password_hash text        CHECK (password_hash IS NULL OR password_hash LIKE '$argon2id$%'),
    published     boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    UNIQUE (tenant_id, id),
    CONSTRAINT status_pages_deleted_unpublished CHECK (deleted_at IS NULL OR NOT published)
);
CREATE UNIQUE INDEX status_pages_slug_idx ON status_pages (slug) WHERE deleted_at IS NULL;
CREATE INDEX status_pages_tenant_idx ON status_pages (tenant_id) WHERE deleted_at IS NULL;
-- Soft delete only; jobs reads.
REVOKE DELETE ON status_pages FROM maiak_api;
REVOKE INSERT, UPDATE, DELETE ON status_pages FROM maiak_worker;
ALTER TABLE status_pages ENABLE ROW LEVEL SECURITY;
ALTER TABLE status_pages FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON status_pages
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TABLE status_page_components (
    id           uuid        PRIMARY KEY,
    tenant_id    uuid        NOT NULL,
    page_id      uuid        NOT NULL,
    monitor_id   uuid        NOT NULL,
    public_name  text        NOT NULL CHECK (char_length(public_name) BETWEEN 1 AND 60),
    group_name   text        CHECK (group_name IS NULL OR char_length(group_name) BETWEEN 1 AND 60),
    position     integer     NOT NULL CHECK (position BETWEEN 0 AND 999),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (page_id, monitor_id),
    UNIQUE (page_id, position),
    FOREIGN KEY (tenant_id, page_id) REFERENCES status_pages (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
);
CREATE INDEX status_page_components_monitor_idx ON status_page_components (tenant_id, monitor_id);
-- jobs removes the components of deleted monitors (ADR-0103) and changes nothing else.
REVOKE INSERT, UPDATE ON status_page_components FROM maiak_worker;
ALTER TABLE status_page_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE status_page_components FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON status_page_components
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The payload is the allow-listed projection built by internal/statuspages (Snapshot); the
-- golden test there fixes its keys.
CREATE TABLE status_page_snapshots (
    page_id      uuid        PRIMARY KEY,
    tenant_id    uuid        NOT NULL,
    slug         text        NOT NULL,
    payload      jsonb       NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    etag         text        NOT NULL CHECK (etag ~ '^[0-9a-f]{32}$'),
    generated_at timestamptz NOT NULL,
    FOREIGN KEY (tenant_id, page_id) REFERENCES status_pages (tenant_id, id) ON DELETE CASCADE
);
ALTER TABLE status_page_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE status_page_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON status_page_snapshots
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Read-only policies for the two definer functions below (they run as maiak_owner before any
-- tenant is known).
CREATE POLICY owner_statuspage_read ON status_pages FOR SELECT TO maiak_owner USING (true);
CREATE POLICY owner_statuspage_read ON status_page_snapshots FOR SELECT TO maiak_owner USING (true);

-- jobs: the published pages of live, active tenants, to rebuild their snapshots in tenant
-- transactions of their own.
-- +goose StatementBegin
CREATE FUNCTION status_page_published_pages()
RETURNS TABLE (tenant_id uuid, page_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT p.tenant_id, p.id
    FROM status_pages AS p
    JOIN tenants AS t ON t.id = p.tenant_id
    WHERE p.published AND p.deleted_at IS NULL
      AND t.status = 'active' AND t.deleted_at IS NULL
    ORDER BY p.tenant_id, p.id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION status_page_published_pages() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION status_page_published_pages() TO maiak_worker;

-- statuspage: the prepared snapshot of a published page of a live, active tenant, with the
-- password hash when the page is protected. Nothing for an unknown, unpublished or deleted page,
-- a suspended or deleted tenant, or a snapshot left from an older slug: the renderer answers all
-- of them with the same 404 (ADR-0103).
-- +goose StatementBegin
CREATE FUNCTION status_page_snapshot(p_slug text)
RETURNS TABLE (payload jsonb, etag text, generated_at timestamptz, password_hash text)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT s.payload, s.etag, s.generated_at, p.password_hash
    FROM status_pages AS p
    JOIN status_page_snapshots AS s ON s.tenant_id = p.tenant_id AND s.page_id = p.id AND s.slug = p.slug
    JOIN tenants AS t ON t.id = p.tenant_id
    WHERE p.slug = p_slug
      AND p.published AND p.deleted_at IS NULL
      AND t.status = 'active' AND t.deleted_at IS NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION status_page_snapshot(text) FROM PUBLIC;
-- The role is created by `maiakctl db bootstrap`, never by a migration (ADR-0014). When the
-- bootstrap has not created it yet, it grants EXECUTE itself once it does.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'maiak_statuspage') THEN
        GRANT EXECUTE ON FUNCTION status_page_snapshot(text) TO maiak_statuspage;
    END IF;
END
$$;
-- +goose StatementEnd

-- Plan key max_status_pages (ADR-0103: 1 on every plan). The previous release ignores it.
UPDATE plans SET limits = limits || '{"max_status_pages": 1}'::jsonb;
