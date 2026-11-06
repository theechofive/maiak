-- name: ListProjects :many
-- Live projects with the counts the list shows (monitors and channels are owned by monitors and
-- notify; these are read-only counts).
SELECT p.id, p.name, p.slug, p.is_default, p.all_members, p.created_at, p.updated_at,
       (SELECT count(*) FROM monitors AS m
        WHERE m.tenant_id = p.tenant_id AND m.project_id = p.id AND m.deleted_at IS NULL)::bigint AS monitor_count,
       (SELECT count(*) FROM notification_channels AS c
        WHERE c.tenant_id = p.tenant_id AND c.project_id = p.id AND c.deleted_at IS NULL)::bigint AS channel_count
FROM projects AS p
WHERE p.tenant_id = @tenant_id AND p.deleted_at IS NULL
ORDER BY p.is_default DESC, lower(p.name), p.id;

-- name: LockProject :one
-- Serialises changes of one project (rename, delete, members) and deletes against new monitors
-- and channels, which take FOR KEY SHARE (LockProjectForUse).
SELECT id, name, slug, is_default, all_members
FROM projects
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL
FOR UPDATE;

-- name: LockProjectForUse :one
-- A monitor or channel is created in or moved to the project: the project must stay live until
-- this transaction commits.
SELECT id
FROM projects
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL
FOR KEY SHARE;

-- name: LockDefaultProject :one
SELECT id
FROM projects
WHERE tenant_id = @tenant_id AND is_default
FOR KEY SHARE;

-- name: InsertProject :exec
INSERT INTO projects (id, tenant_id, name, slug)
VALUES (@id, @tenant_id, @name, @slug);

-- name: UpdateProject :exec
UPDATE projects
SET name = @name, slug = @slug, updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: SoftDeleteProject :exec
UPDATE projects
SET deleted_at = now(), updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND NOT is_default AND deleted_at IS NULL;

-- name: CountProjectContents :one
SELECT (SELECT count(*) FROM monitors AS m
        WHERE m.tenant_id = @tenant_id AND m.project_id = @project_id::uuid AND m.deleted_at IS NULL)::bigint AS monitors,
       (SELECT count(*) FROM notification_channels AS c
        WHERE c.tenant_id = @tenant_id AND c.project_id = @project_id AND c.deleted_at IS NULL)::bigint AS channels;

-- name: DeleteProjectMembers :execrows
DELETE FROM project_members
WHERE tenant_id = @tenant_id AND project_id = @project_id;

-- name: ListUserProjects :many
-- The projects a member or viewer belongs to, with its override: every project with
-- all_members, and the projects that list the user.
SELECT p.id, pm.role
FROM projects AS p
LEFT JOIN project_members AS pm
       ON pm.tenant_id = p.tenant_id AND pm.project_id = p.id AND pm.user_id = @user_id
WHERE p.tenant_id = @tenant_id AND p.deleted_at IS NULL
  AND (p.all_members OR pm.user_id IS NOT NULL);

-- name: ListProjectMembers :many
-- Owners and admins (they see every project), the listed members, and for an all_members project
-- every member of the organization. Reads memberships and users (owned by auth) read-only.
SELECT m.user_id, u.email::text AS login, u.display_name, m.role AS organization_role,
       pm.role AS project_role, (pm.user_id IS NOT NULL)::boolean AS explicit
FROM memberships AS m
JOIN users AS u ON u.id = m.user_id
LEFT JOIN project_members AS pm
       ON pm.tenant_id = m.tenant_id AND pm.project_id = @project_id AND pm.user_id = m.user_id
WHERE m.tenant_id = @tenant_id
  AND (@all_members::boolean OR pm.user_id IS NOT NULL OR m.role IN ('owner', 'admin'))
ORDER BY m.created_at, m.user_id;

-- name: GetOrganizationRole :one
SELECT role
FROM memberships
WHERE tenant_id = @tenant_id AND user_id = @user_id;

-- name: UpsertProjectMember :exec
INSERT INTO project_members (tenant_id, project_id, user_id, role)
VALUES (@tenant_id, @project_id, @user_id, sqlc.narg('role'))
ON CONFLICT (tenant_id, project_id, user_id) DO UPDATE SET role = EXCLUDED.role, updated_at = now();

-- name: DeleteProjectMember :execrows
DELETE FROM project_members
WHERE tenant_id = @tenant_id AND project_id = @project_id AND user_id = @user_id;

-- name: SlugInUse :one
SELECT EXISTS (
    SELECT 1 FROM projects
    WHERE tenant_id = @tenant_id AND slug = @slug AND deleted_at IS NULL AND id <> @except_id
)::boolean;
