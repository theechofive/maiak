-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Plans and quotas (P-01, ADR-0093). Plans are global (not tenant-scoped), like probe_locations:
-- every tenant may read them, only migrations and the owner change them. Owning module:
-- internal/plans. limits holds max_monitors, max_heartbeats and max_channels (non-negative
-- integers); tenants.limits overrides them key by key.
CREATE TABLE plans (
    key         text        PRIMARY KEY CHECK (key ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    limits      jsonb       NOT NULL CHECK (jsonb_typeof(limits) = 'object'),
    created_at  timestamptz NOT NULL DEFAULT now()
);
REVOKE INSERT, UPDATE, DELETE ON plans FROM maiak_api, maiak_worker;

INSERT INTO plans (key, name, limits) VALUES
    ('free', 'Free', '{"max_monitors": 3, "max_heartbeats": 1, "max_channels": 1}'),
    ('internal', 'Internal', '{"max_monitors": 1000, "max_heartbeats": 100, "max_channels": 10}');

-- Expand only: the previous app version ignores the column, and tenants it creates get the
-- default. The constant default makes the column add a catalog change; the foreign key check
-- scans the (small) tenants table once.
ALTER TABLE tenants ADD COLUMN plan_key text NOT NULL DEFAULT 'free' REFERENCES plans (key);

-- The owner's tenant keeps generous limits (it had 50 monitors before plans existed).
UPDATE tenants SET plan_key = 'internal', updated_at = now() WHERE slug = 'default';
