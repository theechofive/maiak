-- name: GetUserByEmail :one
SELECT id, email, password_hash, display_name, status, is_platform_admin
FROM users
WHERE email = @email;

-- name: GetUserByID :one
SELECT id, email, display_name, status, is_platform_admin
FROM users
WHERE id = @id;

-- name: InsertUserIfMissing :exec
INSERT INTO users (id, email, password_hash, display_name)
VALUES (@id, @email, @password_hash, @display_name)
ON CONFLICT (email) DO NOTHING;

-- name: UpdateUserPasswordHash :execrows
UPDATE users
SET password_hash = @password_hash, updated_at = now()
WHERE id = @id AND password_hash IS DISTINCT FROM @password_hash;

-- name: UpsertMembership :exec
INSERT INTO memberships (tenant_id, user_id, role)
VALUES (@tenant_id, @user_id, @role)
ON CONFLICT (tenant_id, user_id) DO UPDATE
SET role = EXCLUDED.role
WHERE memberships.role <> EXCLUDED.role;

-- name: GetMembershipRole :one
SELECT role
FROM memberships
WHERE tenant_id = @tenant_id AND user_id = @user_id;

-- name: InsertSession :exec
INSERT INTO sessions (id, user_id, tenant_id, token_hash, csrf_token, created_at, expires_at, last_seen_at, ip, user_agent)
VALUES (@id, @user_id, @tenant_id, @token_hash, @csrf_token, @created_at, @expires_at, @created_at, @ip, @user_agent);

-- name: TouchSession :exec
UPDATE sessions
SET last_seen_at = @now
WHERE id = @id AND last_seen_at < @stale_before;

-- name: DeleteSession :execrows
DELETE FROM sessions
WHERE id = @id;

-- name: ActivateUser :execrows
UPDATE users
SET status = 'active', updated_at = now()
WHERE id = @id AND status <> 'active';

-- name: DisableUser :exec
UPDATE users
SET status = 'disabled', updated_at = now()
WHERE id = @id;

-- name: ListOtherActiveOwners :many
SELECT m.user_id
FROM memberships AS m
JOIN users AS u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id AND m.role = 'owner' AND m.user_id <> @user_id AND u.status = 'active'
ORDER BY m.user_id;

-- name: InsertUser :exec
-- Registration: a duplicate email fails on users_email_key, which the caller maps to email-taken.
INSERT INTO users (id, email, password_hash, display_name)
VALUES (@id, @email, @password_hash, @display_name);

-- name: InsertMembership :exec
INSERT INTO memberships (tenant_id, user_id, role)
VALUES (@tenant_id, @user_id, @role);
