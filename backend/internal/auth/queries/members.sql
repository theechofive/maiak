-- Members and invitations (T-01, ADR-0102). Every query runs in a tenant transaction.

-- name: ListMembers :many
SELECT m.user_id, u.email, u.display_name, u.status, u.is_platform_admin, m.role, m.created_at
FROM memberships AS m
JOIN users AS u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id
ORDER BY m.created_at, m.user_id;

-- name: LockMember :one
-- The membership a role change or removal acts on, locked until the end of the transaction.
SELECT m.role, m.created_at, u.email, u.display_name, u.status, u.is_platform_admin
FROM memberships AS m
JOIN users AS u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id AND m.user_id = @user_id
FOR UPDATE OF m;

-- name: CountOtherActiveOwners :one
SELECT count(*)
FROM memberships AS m
JOIN users AS u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id AND m.role = 'owner' AND m.user_id <> @user_id AND u.status = 'active';

-- name: UpdateMembershipRole :execrows
UPDATE memberships
SET role = @role
WHERE tenant_id = @tenant_id AND user_id = @user_id;

-- name: DeleteMembership :execrows
-- The user's sessions in the tenant go with it (sessions reference memberships ON DELETE CASCADE).
DELETE FROM memberships
WHERE tenant_id = @tenant_id AND user_id = @user_id;

-- name: InsertInvitation :exec
INSERT INTO invitations (id, tenant_id, role, label, token_hash, created_by, created_at, expires_at)
VALUES (@id, @tenant_id, @role, @label, @token_hash, @created_by, @created_at, @expires_at);

-- name: ListInvitations :many
SELECT i.id, i.role, i.label, i.created_at, i.expires_at, i.accepted_at, i.revoked_at,
       c.email AS created_by_login, a.email AS accepted_by_login
FROM invitations AS i
JOIN users AS c ON c.id = i.created_by
LEFT JOIN users AS a ON a.id = i.accepted_by
WHERE i.tenant_id = @tenant_id
ORDER BY i.created_at DESC, i.id DESC
LIMIT 200;

-- name: LockInvitationByHash :one
SELECT id, role, expires_at, accepted_at, revoked_at
FROM invitations
WHERE tenant_id = @tenant_id AND token_hash = @token_hash
FOR UPDATE;

-- name: LockInvitation :one
SELECT id, role, expires_at, accepted_at, revoked_at
FROM invitations
WHERE tenant_id = @tenant_id AND id = @id
FOR UPDATE;

-- name: MarkInvitationAccepted :execrows
UPDATE invitations
SET accepted_at = @now, accepted_by = @user_id
WHERE tenant_id = @tenant_id AND id = @id AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: RevokeInvitation :execrows
UPDATE invitations
SET revoked_at = @now
WHERE tenant_id = @tenant_id AND id = @id AND accepted_at IS NULL AND revoked_at IS NULL;
