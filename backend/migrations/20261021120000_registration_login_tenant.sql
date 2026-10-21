-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Registration (P-02, ADR-0009 amendment 2026-10-21): a login no longer always binds to the
-- default tenant. A registered user is the owner of their own tenant, so the login must find the
-- user's membership before the tenant is known, which the memberships policy forbids. Same
-- pattern as auth_lookup_session: a SECURITY DEFINER function owned by maiak_owner, allowed by a
-- read-only policy limited to the owner role. Expand only: the previous app version never calls it.
CREATE POLICY owner_membership_lookup ON memberships FOR SELECT TO maiak_owner USING (true);

-- The tenant a login binds to: the user's oldest membership in a tenant that is not deleted, or
-- NULL. It returns only a tenant id for a user id the caller already resolved from the
-- credentials; it does not reveal roles or other members. Suspended tenants are returned on
-- purpose: the login refuses them after audit (tenants.status is checked in the login transaction).
-- +goose StatementBegin
CREATE FUNCTION auth_login_tenant(p_user_id uuid)
RETURNS uuid
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT m.tenant_id
    FROM memberships AS m
    JOIN tenants AS t ON t.id = m.tenant_id
    WHERE m.user_id = p_user_id
      AND t.deleted_at IS NULL
    ORDER BY m.created_at, m.tenant_id
    LIMIT 1
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION auth_login_tenant(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_login_tenant(uuid) TO maiak_api;
