-- name: InsertTenantIfMissing :exec
INSERT INTO tenants (id, slug, name)
VALUES (@id, @slug, @name)
ON CONFLICT (slug) DO NOTHING;

-- name: GetTenantBySlug :one
SELECT id, slug, name, status
FROM tenants
WHERE slug = @slug AND deleted_at IS NULL;

-- name: GetTenantByID :one
SELECT id, slug, name, status
FROM tenants
WHERE id = @id AND deleted_at IS NULL;

-- name: LockTenantLimits :one
SELECT limits
FROM tenants
WHERE id = @id AND deleted_at IS NULL
FOR UPDATE;
