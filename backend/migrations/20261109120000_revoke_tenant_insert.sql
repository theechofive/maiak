-- +goose Up
SET LOCAL lock_timeout = '5s';

-- O-07, contract step of P-07 (ADR-0095, closes L-210): tenants are created only through the
-- definer function auth_create_tenant, which picks the plan. The release that inserted tenants
-- directly is long gone, and the current api has no direct INSERT.
REVOKE INSERT ON tenants FROM maiak_api;
