-- +goose Up
SET LOCAL lock_timeout = '5s';

-- P-06 (review of P-05, ADR-0103): a status page address freed by a delete or a rename stays
-- reserved for its organization for 30 days, so another organization cannot take over a link that
-- visitors, feeds and embeds still use. The organization itself may reclaim it at any time.
-- Additive only: the previous release never reads the table or the function.

CREATE TABLE status_page_released_slugs (
    tenant_id    uuid        NOT NULL REFERENCES tenants (id),
    slug         text        NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{2,62}$'),
    released_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, slug)
);
CREATE INDEX status_page_released_slugs_slug_idx ON status_page_released_slugs (slug, released_at);
REVOKE DELETE ON status_page_released_slugs FROM maiak_api;
REVOKE ALL ON status_page_released_slugs FROM maiak_worker;
ALTER TABLE status_page_released_slugs ENABLE ROW LEVEL SECURITY;
ALTER TABLE status_page_released_slugs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON status_page_released_slugs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY owner_slug_hold_read ON status_page_released_slugs FOR SELECT TO maiak_owner USING (true);

-- Whether a slug is held for another organization than the transaction's (app.tenant_id): released
-- by it in the last 30 days. Answers only true or false, never whose it is.
-- +goose StatementBegin
CREATE FUNCTION status_page_slug_held(p_slug text)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT EXISTS (
        SELECT 1 FROM status_page_released_slugs AS r
        WHERE r.slug = p_slug
          AND r.tenant_id IS DISTINCT FROM NULLIF(current_setting('app.tenant_id', true), '')::uuid
          AND r.released_at > now() - interval '30 days')
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION status_page_slug_held(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION status_page_slug_held(text) TO maiak_api;
