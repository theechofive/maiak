-- +goose Up
SET LOCAL lock_timeout = '5s';

-- O-08. Expand only: the previous release ignores the new column, plan and membership column,
-- and auth_login_tenant keeps its signature, owner and grants.
-- Both tables are locked up front in a fixed order, so the migration never upgrades a lock
-- halfway and cannot deadlock with a running transaction (as T-02 does); with lock_timeout it
-- fails fast instead. Both are small, and ADD COLUMN with a constant default is a catalog change.
LOCK TABLE plans, memberships IN ACCESS EXCLUSIVE MODE;

-- Entitlements (ADR-0105): the paid features a plan unlocks, by name. Empty for every plan today;
-- the api reports them on GET /auth/me and internal/entitlements answers Has(feature). The
-- constant default makes the column add a catalog change.
-- Names are slugs: lowercase letters, digits, hyphen and underscore, 1-64 characters, no NULL.
ALTER TABLE plans ADD COLUMN features text[] NOT NULL DEFAULT '{}'
    CONSTRAINT plans_features_names CHECK (
        array_position(features, NULL) IS NULL
        AND (cardinality(features) = 0 OR array_to_string(features, ',') ~ '^[a-z0-9_-]{1,64}(,[a-z0-9_-]{1,64})*$')
    );

-- The self-hosted default plan for the later packaging task: no quotas. "Unlimited" is the
-- largest value a limit may hold (plans.Unlimited, 2147483647). New tenants keep getting `free`
-- (tenants.plan_key default), so SaaS behaviour does not change.
INSERT INTO plans (key, name, limits) VALUES
    ('community', 'Community', '{"max_monitors": 2147483647, "max_heartbeats": 2147483647, "max_channels": 2147483647,
      "max_members": 2147483647, "max_projects": 2147483647, "max_status_pages": 2147483647}')
ON CONFLICT (key) DO NOTHING;

-- L-307: a login enters the organization the user used last. insertSession (internal/auth) sets
-- it whenever a session is created: login, OAuth sign-in, registration, accepting an invitation
-- and switching the organization. NULL (never used since O-08) falls back to the old order.
ALTER TABLE memberships ADD COLUMN last_used_at timestamptz;

-- Same signature, owner and grants: live tenants first, then active before suspended, then the
-- last used organization, then `default` (the system admin), then the oldest membership.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION auth_login_tenant(p_user_id uuid)
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
    ORDER BY (t.deleted_at IS NULL) DESC, (t.status = 'active') DESC, m.last_used_at DESC NULLS LAST,
             (t.slug = 'default') DESC, m.created_at, m.tenant_id
    LIMIT 1
$$;
-- +goose StatementEnd
