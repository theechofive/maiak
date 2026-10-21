-- name: InsertTenantIfMissing :exec
-- An existing tenant keeps its plan: a restart never changes it.
INSERT INTO tenants (id, slug, name, plan_key)
VALUES (@id, @slug, @name, @plan_key)
ON CONFLICT (slug) DO NOTHING;

-- name: GetTenantBySlug :one
SELECT id, slug, name, status
FROM tenants
WHERE slug = @slug AND deleted_at IS NULL;

-- name: GetTenantByID :one
SELECT id, slug, name, status
FROM tenants
WHERE id = @id AND deleted_at IS NULL;

-- name: LockTenantQuota :one
SELECT plan_key, limits
FROM tenants
WHERE id = @id AND deleted_at IS NULL
FOR UPDATE;

-- name: GetTenantQuota :one
SELECT plan_key, limits
FROM tenants
WHERE id = @id AND deleted_at IS NULL;
