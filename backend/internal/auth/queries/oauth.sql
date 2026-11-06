-- OAuth identities and Workspace domains (O-01, ADR-0104). auth_identities is global like users
-- (no tenant_id, no RLS); org_auth_domains is a tenant table.

-- name: GetIdentity :one
SELECT i.user_id, u.status, u.is_platform_admin
FROM auth_identities AS i
JOIN users AS u ON u.id = i.user_id
WHERE i.provider = @provider AND i.subject = @subject;

-- name: TouchIdentity :exec
UPDATE auth_identities
SET email = @email, hosted_domain = @hosted_domain, last_login_at = @now
WHERE provider = @provider AND subject = @subject;

-- name: InsertIdentity :exec
INSERT INTO auth_identities (user_id, provider, subject, email, hosted_domain, created_at, last_login_at)
VALUES (@user_id, @provider, @subject, @email, @hosted_domain, @now, @now);

-- name: ListIdentities :many
SELECT provider, email, created_at, last_login_at
FROM auth_identities
WHERE user_id = @user_id
ORDER BY provider;

-- name: DeleteIdentity :execrows
DELETE FROM auth_identities
WHERE user_id = @user_id AND provider = @provider;

-- name: HasGoogleDomain :one
-- The proof of control for claiming a domain: the user's Google identity signed in or was linked
-- with that Workspace domain (hd) not before @not_before (a fresh sign-in, O-04).
SELECT EXISTS (
    SELECT 1 FROM auth_identities
    WHERE user_id = @user_id AND provider = 'google' AND hosted_domain = @domain
      AND last_login_at >= @not_before
);

-- name: LockUserForIdentities :one
-- Serialises link and unlink of one user, so two unlinks cannot both pass the last-method rule.
SELECT password_hash, status, is_platform_admin, email
FROM users
WHERE id = @id
FOR UPDATE;

-- name: InsertOAuthUser :exec
-- A user created by an OAuth sign-in: no password, the provider's verified email.
INSERT INTO users (id, email, display_name, email_verified_at)
VALUES (@id, @email, @display_name, @verified_at);

-- name: MarkEmailVerified :exec
UPDATE users
SET email_verified_at = @verified_at
WHERE id = @id AND email_verified_at IS NULL;

-- name: ListAuthDomains :many
SELECT id, domain, default_role, enabled, created_at, updated_at
FROM org_auth_domains
WHERE tenant_id = @tenant_id
ORDER BY domain;

-- name: CountAuthDomains :one
SELECT count(*)
FROM org_auth_domains
WHERE tenant_id = @tenant_id;

-- name: InsertAuthDomain :exec
INSERT INTO org_auth_domains (id, tenant_id, domain, default_role, enabled, created_by, created_at, updated_at)
VALUES (@id, @tenant_id, @domain, @default_role, @enabled, @created_by, @now, @now);

-- name: LockAuthDomain :one
SELECT id, domain, default_role, enabled, created_at, updated_at
FROM org_auth_domains
WHERE tenant_id = @tenant_id AND id = @id
FOR UPDATE;

-- name: UpdateAuthDomain :exec
UPDATE org_auth_domains
SET default_role = @default_role, enabled = @enabled, updated_at = @now
WHERE tenant_id = @tenant_id AND id = @id;

-- name: DeleteAuthDomain :execrows
DELETE FROM org_auth_domains
WHERE tenant_id = @tenant_id AND id = @id;

-- name: ShareAuthDomainByName :one
-- The domain a sign-in joins through, re-read in the joining transaction (the lookup ran before
-- the tenant was known).
SELECT default_role
FROM org_auth_domains
WHERE tenant_id = @tenant_id AND domain = @domain AND enabled
FOR SHARE;

-- name: GetUserSignIn :one
SELECT password_hash, email
FROM users
WHERE id = @id;
