-- +goose Up
SET LOCAL lock_timeout = '5s';

-- T-01 (ADR-0102): invitations, roles, the organization switcher and owner-only organization
-- delete. Expand only: the previous app version never reads the new table, ignores the new plan
-- key (plans.Resolve reads only the keys it knows) and never calls the new functions.

-- An invitation into an existing tenant. The token is a bearer secret (R-213): only its SHA-256
-- is stored, like session and heartbeat tokens. An invitation is open while accepted_at,
-- revoked_at are NULL and expires_at is in the future; rows are kept as history (no DELETE).
CREATE TABLE invitations (
    id           uuid        PRIMARY KEY,
    tenant_id    uuid        NOT NULL REFERENCES tenants (id),
    role         text        NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
    label        text        NOT NULL DEFAULT '' CHECK (length(label) <= 80),
    token_hash   bytea       NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_by   uuid        NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    accepted_at  timestamptz,
    accepted_by  uuid        REFERENCES users (id),
    revoked_at   timestamptz,
    UNIQUE (tenant_id, id),
    CONSTRAINT invitations_accepted_pair CHECK ((accepted_at IS NULL) = (accepted_by IS NULL)),
    CONSTRAINT invitations_single_outcome CHECK (accepted_at IS NULL OR revoked_at IS NULL),
    CONSTRAINT invitations_expiry CHECK (expires_at > created_at)
);
CREATE INDEX invitations_tenant_created_idx ON invitations (tenant_id, created_at DESC);
REVOKE ALL ON invitations FROM maiak_worker;
REVOKE DELETE ON invitations FROM maiak_api;

ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invitations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
-- auth_invitation_lookup finds an invitation by token hash before the tenant is known, like
-- owner_session_lookup. Read-only and limited to the owner role.
CREATE POLICY owner_invitation_lookup ON invitations FOR SELECT TO maiak_owner USING (true);

-- The tenant and role of an open invitation, for an exact 32-byte token hash. Unknown, expired,
-- accepted and revoked tokens, and invitations of suspended or deleted tenants, all return no
-- row, so the caller can give one constant answer. The acceptance itself runs in a normal tenant
-- transaction once the tenant is known (row lock on the invitation, ADR-0102).
-- +goose StatementBegin
CREATE FUNCTION auth_invitation_lookup(p_token_hash bytea)
RETURNS TABLE (
    invitation_id  uuid,
    tenant_id      uuid,
    tenant_name    text,
    role           text,
    expires_at     timestamptz
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT i.id, i.tenant_id, t.name, i.role, i.expires_at
    FROM invitations AS i
    JOIN tenants AS t ON t.id = i.tenant_id
    WHERE i.token_hash = p_token_hash
      AND length(p_token_hash) = 32
      AND i.accepted_at IS NULL
      AND i.revoked_at IS NULL
      AND i.expires_at > now()
      AND t.status = 'active'
      AND t.deleted_at IS NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION auth_invitation_lookup(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_invitation_lookup(bytea) TO maiak_api;

-- The organizations of a user for the switcher (closes L-190): every membership in a tenant that
-- is not deleted, with the tenant's name and status and the user's role. Cross-tenant read of
-- one user's own memberships only, through owner_membership_lookup.
-- +goose StatementBegin
CREATE FUNCTION auth_user_organizations(p_user_id uuid)
RETURNS TABLE (
    tenant_id  uuid,
    slug       text,
    name       text,
    status     text,
    role       text,
    joined_at  timestamptz
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT t.id, t.slug, t.name, t.status, m.role, m.created_at
    FROM memberships AS m
    JOIN tenants AS t ON t.id = m.tenant_id
    WHERE m.user_id = p_user_id
      AND t.deleted_at IS NULL
    ORDER BY lower(t.name), t.id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION auth_user_organizations(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_user_organizations(uuid) TO maiak_api;

-- Owner-only soft delete of the caller's own organization (ADR-0102, same effect as
-- admin_delete_tenant: deleted_at and status suspended). The api role cannot update tenants, so
-- this is a definer function. It acts only on the tenant of the caller's transaction
-- (app.tenant_id), re-checks that the actor is an active owner there, refuses the default tenant
-- (MKO01) and audits organization.deleted in the same transaction. Returns false when the tenant
-- is already deleted.
-- +goose StatementBegin
CREATE FUNCTION org_delete_tenant(p_actor uuid, p_audit_id uuid, p_ip inet)
RETURNS boolean
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_tenant  uuid := NULLIF(current_setting('app.tenant_id', true), '')::uuid;
    v_slug    text;
    v_deleted timestamptz;
BEGIN
    IF v_tenant IS NULL THEN
        RAISE EXCEPTION 'org_delete_tenant: no tenant in the transaction' USING ERRCODE = 'MKO02';
    END IF;
    SELECT t.slug, t.deleted_at INTO v_slug, v_deleted FROM tenants AS t WHERE t.id = v_tenant FOR UPDATE;
    IF NOT FOUND OR v_deleted IS NOT NULL THEN
        RETURN false;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM memberships AS m JOIN users AS u ON u.id = m.user_id
        WHERE m.tenant_id = v_tenant AND m.user_id = p_actor AND m.role = 'owner' AND u.status = 'active'
    ) THEN
        RAISE EXCEPTION 'org_delete_tenant: actor is not an owner' USING ERRCODE = 'MKO02';
    END IF;
    IF v_slug = 'default' THEN
        RAISE EXCEPTION 'org_delete_tenant: the default tenant cannot be deleted' USING ERRCODE = 'MKO01';
    END IF;
    UPDATE tenants SET deleted_at = now(), status = 'suspended', updated_at = now() WHERE id = v_tenant;
    INSERT INTO audit_log (id, tenant_id, actor_user_id, action, entity_type, entity_id, metadata, ip)
    VALUES (p_audit_id, v_tenant, p_actor, 'organization.deleted', 'tenant', v_tenant::text, '{}'::jsonb, p_ip);
    RETURN true;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION org_delete_tenant(uuid, uuid, inet) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION org_delete_tenant(uuid, uuid, inet) TO maiak_api;

-- Plan key max_members (memberships plus open invitations, ADR-0102).
UPDATE plans SET limits = limits || '{"max_members": 3}'::jsonb WHERE key = 'free';
UPDATE plans SET limits = limits || '{"max_members": 50}'::jsonb WHERE key = 'internal';
