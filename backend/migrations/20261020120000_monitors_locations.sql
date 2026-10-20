-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Probe locations are global (not tenant-scoped): every tenant may read them, only migrations
-- and the owner change them. Owning module: internal/scheduler.
CREATE TABLE probe_locations (
    id          uuid        PRIMARY KEY,
    code        text        NOT NULL UNIQUE CHECK (code ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    provider    text        NOT NULL CHECK (length(provider) BETWEEN 1 AND 100),
    region      text        NOT NULL CHECK (length(region) BETWEEN 1 AND 100),
    kind        text        NOT NULL CHECK (kind IN ('platform', 'agent')),
    status      text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at  timestamptz NOT NULL DEFAULT now()
);
REVOKE INSERT, UPDATE, DELETE ON probe_locations FROM maiak_api, maiak_worker;

-- The single MVP location: the worker in klr-platform. The id is fixed so every environment has
-- the same one.
INSERT INTO probe_locations (id, code, name, provider, region, kind)
VALUES ('01920000-0000-7000-8000-000000000001', 'home-eu-central-1', 'Home (eu-central-1)', 'aws', 'eu-central-1', 'platform');

-- Owning module: internal/monitors. Check parameters live in config (http: url, expectedStatus)
-- so other check types need no schema change. Soft delete only.
CREATE TABLE monitors (
    id                 uuid        PRIMARY KEY,
    tenant_id          uuid        NOT NULL REFERENCES tenants (id),
    name               text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    type               text        NOT NULL DEFAULT 'http' CHECK (type IN ('http')),
    config             jsonb       NOT NULL CHECK (jsonb_typeof(config) = 'object'),
    interval_seconds   integer     NOT NULL DEFAULT 60 CHECK (interval_seconds > 0),
    timeout_seconds    integer     NOT NULL DEFAULT 10 CHECK (timeout_seconds > 0 AND timeout_seconds < interval_seconds),
    failure_threshold  integer     NOT NULL DEFAULT 2 CHECK (failure_threshold BETWEEN 1 AND 3),
    notify_slack       boolean     NOT NULL DEFAULT true,
    paused             boolean     NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    deleted_at         timestamptz,
    UNIQUE (tenant_id, id),
    -- MVP limits (ADR-0016) as named constraints, so relaxing them is one DROP CONSTRAINT.
    CONSTRAINT monitors_mvp_interval CHECK (interval_seconds = 60),
    CONSTRAINT monitors_mvp_timeout CHECK (timeout_seconds = 10)
);
-- Listing and the limit count read live monitors of one tenant.
CREATE INDEX monitors_tenant_live_idx ON monitors (tenant_id, created_at) WHERE deleted_at IS NULL;
REVOKE DELETE ON monitors FROM maiak_api;
REVOKE INSERT, UPDATE, DELETE ON monitors FROM maiak_worker;

-- Which locations run which monitor. Owning module: internal/monitors.
CREATE TABLE monitor_locations (
    tenant_id    uuid        NOT NULL,
    monitor_id   uuid        NOT NULL,
    location_id  uuid        NOT NULL REFERENCES probe_locations (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (monitor_id, location_id),
    UNIQUE (tenant_id, monitor_id, location_id),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
);
CREATE INDEX monitor_locations_location_idx ON monitor_locations (location_id);
REVOKE UPDATE, DELETE ON monitor_locations FROM maiak_api;
REVOKE INSERT, UPDATE, DELETE ON monitor_locations FROM maiak_worker;

-- Schedule and per-location state; very high update rate. Owning module: internal/scheduler.
-- A free lease is lease_token IS NULL; an expired one (crashed worker) is found through the
-- second partial index. fillfactor leaves room for in-page updates.
CREATE TABLE monitor_location_state (
    tenant_id             uuid        NOT NULL,
    monitor_id            uuid        NOT NULL,
    location_id           uuid        NOT NULL,
    status                text        NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown', 'up', 'down')),
    consecutive_failures  integer     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    next_run_at           timestamptz NOT NULL,
    lease_token           uuid,
    lease_until           timestamptz,
    last_checked_at       timestamptz,
    PRIMARY KEY (monitor_id, location_id),
    FOREIGN KEY (tenant_id, monitor_id, location_id) REFERENCES monitor_locations (tenant_id, monitor_id, location_id),
    CHECK ((lease_token IS NULL) = (lease_until IS NULL))
) WITH (fillfactor = 70);
CREATE INDEX monitor_location_state_due_idx ON monitor_location_state (next_run_at) WHERE lease_token IS NULL;
CREATE INDEX monitor_location_state_lease_idx ON monitor_location_state (lease_until) WHERE lease_token IS NOT NULL;
REVOKE DELETE ON monitor_location_state FROM maiak_api, maiak_worker;
REVOKE INSERT ON monitor_location_state FROM maiak_worker;

-- Aggregated status of a monitor. Owning module: internal/monitors (the worker's state machine
-- updates it in the result transaction). current_incident_id gets its foreign key when the
-- incidents table exists (B-11).
CREATE TABLE monitor_state (
    tenant_id             uuid        NOT NULL,
    monitor_id            uuid        PRIMARY KEY,
    status                text        NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown', 'up', 'down')),
    status_changed_at     timestamptz NOT NULL DEFAULT now(),
    last_error_kind       text        CHECK (length(last_error_kind) BETWEEN 1 AND 64),
    last_checked_at       timestamptz,
    tls_expires_at        timestamptz,
    tls_warned_not_after  timestamptz,
    current_incident_id   uuid,
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
) WITH (fillfactor = 80);
REVOKE DELETE ON monitor_state FROM maiak_api, maiak_worker;
REVOKE INSERT ON monitor_state FROM maiak_worker;

ALTER TABLE monitors ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitors
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE monitor_locations ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitor_locations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitor_locations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE monitor_location_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitor_location_state FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitor_location_state
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE monitor_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitor_state FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitor_state
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
