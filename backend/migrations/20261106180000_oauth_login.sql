-- +goose Up
SET LOCAL lock_timeout = '5s';

-- O-01 (ADR-0104): OAuth login with Google and GitHub, identities, Google Workspace domains.
-- Expand only: the previous app version never reads the new columns, the new table or the new
-- function, inserts users without email_verified_at (NULL is "not verified") and never writes
-- auth_identities.

-- When the user's email was verified by an identity provider (users created by an OAuth login,
-- or a password user who linked a provider with the same verified address). NULL: never verified
-- (R-120).
ALTER TABLE users ADD COLUMN email_verified_at timestamptz;
GRANT INSERT (email_verified_at) ON users TO maiak_api;
GRANT UPDATE (email_verified_at) ON users TO maiak_api;

-- auth_identities has been empty since the first migration (L-007), so the new constraints apply
-- to no existing row. No provider tokens are stored: only the stable subject, the verified email
-- the provider reported last and, for Google, the Workspace domain (hd) of the last sign-in,
-- which is the proof of control for claiming a domain.
ALTER TABLE auth_identities
    ADD COLUMN email          citext      CHECK (length(email) BETWEEN 3 AND 320),
    ADD COLUMN hosted_domain  text        CHECK (length(hosted_domain) BETWEEN 1 AND 253),
    ADD COLUMN last_login_at  timestamptz,
    ADD CONSTRAINT auth_identities_known_provider CHECK (provider IN ('google', 'github'));
-- One identity per provider and user: linking a second Google account means unlinking first.
CREATE UNIQUE INDEX auth_identities_user_provider_key ON auth_identities (user_id, provider);
-- The api refreshes what the provider reported at each sign-in; subject and owner never change.
GRANT UPDATE (email, hosted_domain, last_login_at) ON auth_identities TO maiak_api;

-- A Google Workspace domain claimed by an organization: a verified Google account whose hd claim
-- equals the domain joins the organization with default_role on its first sign-in. A domain is
-- claimed by at most one organization in the whole installation (global UNIQUE).
CREATE TABLE org_auth_domains (
    id            uuid        PRIMARY KEY,
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    domain        text        NOT NULL UNIQUE CHECK (
        length(domain) BETWEEN 3 AND 253
        AND domain ~ '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$'),
    default_role  text        NOT NULL DEFAULT 'viewer' CHECK (default_role IN ('admin', 'member', 'viewer')),
    enabled       boolean     NOT NULL DEFAULT true,
    created_by    uuid        NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);
CREATE INDEX org_auth_domains_tenant_idx ON org_auth_domains (tenant_id, domain);
REVOKE ALL ON org_auth_domains FROM maiak_worker;

ALTER TABLE org_auth_domains ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_auth_domains FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON org_auth_domains
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
-- auth_domain_lookup finds the organization of a domain before the tenant is known, like
-- owner_invitation_lookup. Read-only and limited to the owner role.
CREATE POLICY owner_domain_lookup ON org_auth_domains FOR SELECT TO maiak_owner USING (true);

-- The organization and default role of an enabled domain, for an exact lowercase domain. Domains
-- of suspended or deleted organizations return no row.
-- +goose StatementBegin
CREATE FUNCTION auth_domain_lookup(p_domain text)
RETURNS TABLE (
    tenant_id     uuid,
    default_role  text
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT d.tenant_id, d.default_role
    FROM org_auth_domains AS d
    JOIN tenants AS t ON t.id = d.tenant_id
    WHERE d.domain = p_domain
      AND d.enabled
      AND t.status = 'active'
      AND t.deleted_at IS NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION auth_domain_lookup(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_domain_lookup(text) TO maiak_api;
