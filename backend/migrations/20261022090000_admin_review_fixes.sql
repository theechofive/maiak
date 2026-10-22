-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Fixes from the review of the platform admin panel (P-09, ADR-0098). New bodies only: every
-- function keeps its signature, owner, pinned search_path and grants (CREATE OR REPLACE), so the
-- previous app version calls them unchanged. No table, column or grant changes.

-- The login tenant: live before deleted, then active before suspended, then the default tenant,
-- then the oldest membership. A user in an older suspended tenant and a newer active one logs
-- into the active one instead of being told the account is suspended.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION auth_login_tenant(p_user_id uuid)
RETURNS uuid
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT m.tenant_id
    FROM memberships AS m
    JOIN tenants AS t ON t.id = m.tenant_id
    WHERE m.user_id = p_user_id
    ORDER BY (t.deleted_at IS NULL) DESC, (t.status = 'active') DESC, (t.slug = 'default') DESC, m.created_at, m.tenant_id
    LIMIT 1
$$;
-- +goose StatementEnd

-- Unsuspending a tenant expires its Slack deliveries that waited more than 15 minutes: they are
-- marked failed with last_error 'tenant_suspended' instead of alerting about the past all at
-- once. Newer ones are still sent. The unsuspend audit row carries the number expired
-- (deliveries_expired); the suspend audit row is unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION admin_set_tenant_status(p_actor uuid, p_tenant_id uuid, p_status text, p_audit_id uuid, p_ip inet)
RETURNS boolean
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_old     text;
    v_slug    text;
    v_deleted timestamptz;
    v_expired integer;
    v_meta    jsonb;
BEGIN
    PERFORM admin_require_platform_admin(p_actor);
    IF p_status IS NULL OR p_status NOT IN ('active', 'suspended') THEN
        RAISE EXCEPTION 'admin: invalid status' USING ERRCODE = 'MKA04';
    END IF;
    SELECT t.status, t.slug, t.deleted_at INTO v_old, v_slug, v_deleted FROM tenants AS t WHERE t.id = p_tenant_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'admin: tenant not found' USING ERRCODE = 'MKA02';
    END IF;
    IF v_deleted IS NOT NULL THEN
        RAISE EXCEPTION 'admin: tenant is deleted' USING ERRCODE = 'MKA08';
    END IF;
    IF p_status = 'suspended' THEN
        IF v_slug = 'default' THEN
            RAISE EXCEPTION 'admin: the default tenant cannot be suspended' USING ERRCODE = 'MKA05';
        END IF;
        IF EXISTS (SELECT 1 FROM memberships AS m WHERE m.tenant_id = p_tenant_id AND m.user_id = p_actor) THEN
            RAISE EXCEPTION 'admin: cannot suspend a tenant the admin belongs to' USING ERRCODE = 'MKA06';
        END IF;
    END IF;
    IF v_old = p_status THEN
        RETURN false;
    END IF;
    UPDATE tenants SET status = p_status, updated_at = now() WHERE id = p_tenant_id;
    v_meta := jsonb_build_object('from', v_old, 'to', p_status);
    IF p_status = 'active' THEN
        -- owner_delivery_update lets the owner update rows of any tenant; claim_deliveries skips
        -- the tenant until this transaction commits, so no row is in progress.
        UPDATE notification_deliveries AS d
        SET status = 'failed', last_error = 'tenant_suspended'
        WHERE d.tenant_id = p_tenant_id AND d.status = 'pending' AND d.created_at < now() - interval '15 minutes';
        GET DIAGNOSTICS v_expired = ROW_COUNT;
        v_meta := v_meta || jsonb_build_object('deliveries_expired', v_expired);
    END IF;
    PERFORM admin_audit(p_tenant_id, p_actor, p_audit_id,
                        CASE WHEN p_status = 'suspended' THEN 'admin.tenant.suspended' ELSE 'admin.tenant.unsuspended' END,
                        'tenant', p_tenant_id::text, v_meta, p_ip);
    RETURN true;
END
$$;
-- +goose StatementEnd

-- The audit tenant of a password reset is the user's login tenant, now taken from
-- auth_login_tenant itself so both keep the same order.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION admin_reset_user_password(p_actor uuid, p_user_id uuid, p_password_hash text, p_audit_id uuid, p_ip inet)
RETURNS TABLE (
    tenant_id        uuid,
    sessions_revoked integer
)
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    v_admin  boolean;
    v_tenant uuid;
    v_n      integer;
BEGIN
    PERFORM admin_require_platform_admin(p_actor);
    IF p_password_hash IS NULL OR p_password_hash NOT LIKE '$argon2id$%' THEN
        RAISE EXCEPTION 'admin: invalid password hash' USING ERRCODE = 'MKA04';
    END IF;
    SELECT u.is_platform_admin INTO v_admin FROM users AS u WHERE u.id = p_user_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'admin: user not found' USING ERRCODE = 'MKA02';
    END IF;
    IF v_admin THEN
        RAISE EXCEPTION 'admin: a platform admin password is managed by its Secret' USING ERRCODE = 'MKA07';
    END IF;
    v_tenant := auth_login_tenant(p_user_id);
    IF v_tenant IS NULL THEN
        RAISE EXCEPTION 'admin: user has no membership' USING ERRCODE = 'MKA02';
    END IF;
    -- updated_at changes with the hash: a login verified against the old hash is refused when
    -- it creates its session (ADR-0098).
    UPDATE users SET password_hash = p_password_hash, updated_at = now() WHERE id = p_user_id;
    v_n := auth_revoke_user_sessions(p_user_id);
    PERFORM admin_audit(v_tenant, p_actor, p_audit_id, 'admin.user.password_reset', 'user', p_user_id::text,
                        jsonb_build_object('sessions_revoked', v_n), p_ip);
    RETURN QUERY SELECT v_tenant, v_n;
END
$$;
-- +goose StatementEnd

-- Deliveries of deleted tenants are never sent: the claim now marks the pending ones failed
-- once (last_error 'tenant_deleted'), so its scans stop reading them again on every run. This
-- also catches rows whose lease expires after the deletion. Suspended tenants keep waiting.
-- Everything else is unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION claim_deliveries(n integer)
RETURNS TABLE (
    id           uuid,
    tenant_id    uuid,
    kind         text,
    monitor_id   uuid,
    incident_id  uuid,
    channel_id   uuid,
    payload      jsonb,
    attempts     integer,
    created_at   timestamptz,
    claim_token  uuid
)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    UPDATE notification_deliveries AS d
    SET status = CASE WHEN d.attempts + 1 >= 5 THEN 'failed' ELSE 'pending' END,
        attempts = d.attempts + 1,
        last_error = 'lease_expired',
        lease_until = NULL,
        claim_token = NULL
    WHERE d.id IN (
        SELECT e.id FROM notification_deliveries AS e
        WHERE e.status = 'in_progress' AND e.lease_until <= now()
        FOR UPDATE SKIP LOCKED
    );

    UPDATE notification_deliveries AS d
    SET status = 'failed', last_error = 'tenant_deleted'
    WHERE d.id IN (
        SELECT e.id FROM notification_deliveries AS e
        JOIN tenants AS t ON t.id = e.tenant_id
        WHERE e.status = 'pending' AND t.deleted_at IS NOT NULL
        FOR UPDATE OF e SKIP LOCKED
    );

    WITH ready AS (
        SELECT d.id
        FROM notification_deliveries AS d
        WHERE d.status = 'pending'
          AND d.next_attempt_at <= now()
          AND EXISTS (
              SELECT 1 FROM tenants AS t
              WHERE t.id = d.tenant_id AND t.status = 'active' AND t.deleted_at IS NULL
          )
          AND NOT EXISTS (
              SELECT 1
              FROM notification_deliveries AS o
              WHERE o.monitor_id = d.monitor_id
                AND o.status IN ('pending', 'in_progress')
                AND (o.created_at, o.id) < (d.created_at, d.id)
          )
        ORDER BY d.created_at, d.id
        LIMIT least(greatest(n, 0), 100)
        FOR UPDATE OF d SKIP LOCKED
    )
    UPDATE notification_deliveries AS d
    SET status = 'in_progress', lease_until = now() + interval '2 minutes', claim_token = gen_random_uuid()
    FROM ready
    WHERE d.id = ready.id
    RETURNING d.id, d.tenant_id, d.kind, d.monitor_id, d.incident_id, d.channel_id, d.payload, d.attempts, d.created_at,
              d.claim_token
$$;
-- +goose StatementEnd
