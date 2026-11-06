-- Status pages (P-05, ADR-0103). The module owns status_pages, status_page_components and
-- status_page_snapshots; the builder reads monitors, their state, rollups and incidents.

-- name: InsertPage :exec
INSERT INTO status_pages (id, tenant_id, slug, title, description, logo_url, accent_color)
VALUES (@id, @tenant_id, @slug, @title, @description, sqlc.narg(logo_url), sqlc.narg(accent_color));

-- name: ListPages :many
SELECT p.id, p.slug, p.title, p.description, p.logo_url, p.accent_color, (p.password_hash IS NOT NULL)::boolean AS protected,
       p.published, p.created_at, p.updated_at,
       (SELECT count(*) FROM status_page_components AS c
        JOIN monitors AS m ON m.tenant_id = c.tenant_id AND m.id = c.monitor_id AND m.deleted_at IS NULL
        WHERE c.tenant_id = p.tenant_id AND c.page_id = p.id)::integer AS component_count
FROM status_pages AS p
WHERE p.tenant_id = @tenant_id AND p.deleted_at IS NULL
ORDER BY p.created_at, p.id;

-- name: LockPage :one
SELECT id, slug, title, description, logo_url, accent_color, password_hash, published
FROM status_pages
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL
FOR UPDATE;

-- name: GetPageForBuild :one
SELECT id, slug, title, description, logo_url, accent_color, published
FROM status_pages
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: UpdatePage :exec
UPDATE status_pages
SET slug = @slug, title = @title, description = @description, logo_url = sqlc.narg(logo_url),
    accent_color = sqlc.narg(accent_color), published = @published, updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: SetPagePassword :exec
UPDATE status_pages
SET password_hash = sqlc.narg(password_hash), updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: SoftDeletePage :exec
UPDATE status_pages
SET published = false, deleted_at = now(), updated_at = now()
WHERE tenant_id = @tenant_id AND id = @id AND deleted_at IS NULL;

-- name: ListComponents :many
-- Components whose monitor is live (a deleted monitor's component is gone, ADR-0103), in order.
SELECT c.monitor_id, c.public_name, c.group_name, c.position, m.name AS monitor_name, m.type AS monitor_type,
       m.project_id, m.paused, m.created_at AS monitor_created_at, coalesce(ms.status, 'unknown')::text AS monitor_status,
       (SELECT count(*) FROM monitor_location_state AS l
        WHERE l.tenant_id = c.tenant_id AND l.monitor_id = c.monitor_id AND l.status <> 'down' AND l.consecutive_failures > 0)::integer AS failing_locations,
       (SELECT count(*) FROM monitor_location_state AS l
        WHERE l.tenant_id = c.tenant_id AND l.monitor_id = c.monitor_id AND l.status = 'down')::integer AS down_locations
FROM status_page_components AS c
JOIN monitors AS m ON m.tenant_id = c.tenant_id AND m.id = c.monitor_id AND m.deleted_at IS NULL
LEFT JOIN monitor_state AS ms ON ms.tenant_id = c.tenant_id AND ms.monitor_id = c.monitor_id
WHERE c.tenant_id = @tenant_id AND c.page_id = @page_id
ORDER BY c.position;

-- name: DeleteComponents :exec
DELETE FROM status_page_components
WHERE tenant_id = @tenant_id AND page_id = @page_id;

-- name: InsertComponent :exec
INSERT INTO status_page_components (id, tenant_id, page_id, monitor_id, public_name, group_name, position)
VALUES (@id, @tenant_id, @page_id, @monitor_id, @public_name, sqlc.narg(group_name), @position);

-- name: LiveMonitorsOf :many
-- The live monitors among the ids, locked against a concurrent delete (any project: ADR-0103).
SELECT id
FROM monitors
WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]) AND deleted_at IS NULL
FOR KEY SHARE;

-- name: DeleteComponentsOfDeletedMonitors :execrows
DELETE FROM status_page_components AS c
USING monitors AS m
WHERE c.tenant_id = @tenant_id AND m.tenant_id = c.tenant_id AND m.id = c.monitor_id AND m.deleted_at IS NOT NULL;

-- name: DailyCounts :many
-- Checks per UTC day of the given monitors from the hourly rollups (all locations together).
SELECT monitor_id, to_char(hour AT TIME ZONE 'UTC', 'YYYY-MM-DD')::text AS day,
       sum(total)::bigint AS total, sum(ok_count)::bigint AS ok
FROM result_rollups_hourly
WHERE tenant_id = @tenant_id AND monitor_id = ANY(@monitor_ids::uuid[]) AND hour >= @since
GROUP BY monitor_id, day;

-- name: RecentIncidents :many
-- Automatic incidents of the given monitors that are open or ended after since, newest first.
SELECT id, monitor_id, started_at, resolved_at
FROM incidents
WHERE tenant_id = @tenant_id AND monitor_id = ANY(@monitor_ids::uuid[])
  AND (resolved_at IS NULL OR resolved_at >= @since)
ORDER BY started_at DESC, id
LIMIT 500;

-- name: UpsertSnapshot :exec
INSERT INTO status_page_snapshots (page_id, tenant_id, slug, payload, etag, generated_at)
VALUES (@page_id, @tenant_id, @slug, @payload, @etag, @generated_at)
ON CONFLICT (page_id) DO UPDATE
SET slug = EXCLUDED.slug, payload = EXCLUDED.payload, etag = EXCLUDED.etag, generated_at = EXCLUDED.generated_at;

-- name: DeleteSnapshot :exec
DELETE FROM status_page_snapshots
WHERE tenant_id = @tenant_id AND page_id = @page_id;
