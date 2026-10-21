-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Platform admin panel (P-04, ADR-0097). Expand only: the previous app version never reads the
-- new column, inserts users with the four columns that keep their INSERT grant, never calls the
-- new functions, and the new bodies of auth_login_tenant and claim_deliveries keep their
-- signatures, owners and grants (CREATE OR REPLACE).

-- The platform-admin flag. Only auth_sync_platform_admin (called by BootstrapAdmin for the
-- configured system admin) changes it; no API grants or revokes it.
ALTER TABLE users ADD COLUMN is_platform_admin boolean NOT NULL DEFAULT false;

-- users has no RLS, so grants are the only guard: the api may insert users only with the
-- columns registration and the bootstrap write, never with is_platform_admin, and its column
-- UPDATE grant (id, email, password_hash, display_name, status, updated_at) never included it.
REVOKE INSERT ON users FROM maiak_api;
GRANT INSERT (id, email, password_hash, display_name) ON users TO maiak_api;

-- Internal helpers of the admin definer functions. They run as the caller (maiak_owner inside a
-- definer function), are executable by nobody else (default privileges revoke PUBLIC) and are not
-- SECURITY DEFINER themselves, so the schema lint does not list them.

-- Raises MKA01 unless the actor is an active platform admin. Every admin_* function calls it
-- first: the api passes the user of the session, the same trust as app.tenant_id (ADR-0097).
-- +goose StatementBegin
CREATE FUNCTION admin_require_platform_admin(p_actor uuid)
RETURNS void
LANGUAGE plpgsql
STABLE
SET search_path = public, pg_temp
AS $$
BEGIN
    IF p_actor IS NULL OR NOT EXISTS (
        SELECT 1 FROM users AS u WHERE u.id = p_actor AND u.status = 'active' AND u.is_platform_admin
    ) THEN
        RAISE EXCEPTION 'admin: actor is not a platform admin' USING ERRCODE = 'MKA01';
    END IF;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_require_platform_admin(uuid) FROM PUBLIC;

-- Writes an audit row into the target tenant. audit_log forces RLS for its owner too, so the row
-- is inserted under that tenant's app.tenant_id, and the caller's value is restored afterwards
-- (like auth_revoke_user_sessions). Metadata never carries a password or a hash.
-- +goose StatementBegin
CREATE FUNCTION admin_audit(p_tenant_id uuid, p_actor uuid, p_audit_id uuid, p_action text,
                            p_entity_type text, p_entity_id text, p_metadata jsonb, p_ip inet)
RETURNS void
LANGUAGE plpgsql
VOLATILE
SET search_path = public, pg_temp
AS $$
DECLARE
    caller text := current_setting('app.tenant_id', true);
BEGIN
    PERFORM set_config('app.tenant_id', p_tenant_id::text, true);
    INSERT INTO audit_log (id, tenant_id, actor_user_id, action, entity_type, entity_id, metadata, ip)
    VALUES (p_audit_id, p_tenant_id, p_actor, p_action, p_entity_type, p_entity_id, p_metadata, p_ip);
    PERFORM set_config('app.tenant_id', COALESCE(caller, ''), true);
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_audit(uuid, uuid, uuid, text, text, text, jsonb, inet) FROM PUBLIC;

-- Makes p_user_id the only platform admin: sets the flag for it and clears it for every other
-- user (a retired previous admin). Returns the number of users changed. BootstrapAdmin calls it
-- for the configured system admin; it refuses (MKA01) a user that is not an active owner of the
-- default tenant or that has a membership anywhere else, so even the api cannot hand the flag to
-- a registered user. Reads memberships through owner_membership_lookup.
-- +goose StatementBegin
CREATE FUNCTION auth_sync_platform_admin(p_user_id uuid)
RETURNS integer
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    n integer;
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM users AS u
        JOIN memberships AS m ON m.user_id = u.id
        JOIN tenants AS t ON t.id = m.tenant_id
        WHERE u.id = p_user_id AND u.status = 'active' AND m.role = 'owner'
          AND t.slug = 'default' AND t.deleted_at IS NULL
    ) OR EXISTS (
        SELECT 1
        FROM memberships AS m
        JOIN tenants AS t ON t.id = m.tenant_id
        WHERE m.user_id = p_user_id AND t.slug <> 'default'
    ) THEN
        RAISE EXCEPTION 'auth_sync_platform_admin: user is not the owner of the default tenant only'
            USING ERRCODE = 'MKA01';
    END IF;
    UPDATE users AS u
    SET is_platform_admin = (u.id = p_user_id), updated_at = now()
    WHERE u.is_platform_admin IS DISTINCT FROM (u.id = p_user_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION auth_sync_platform_admin(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_sync_platform_admin(uuid) TO maiak_api;

-- Error codes of the admin functions (mapped to HTTP problems in internal/admin):
--   MKA01 the actor is not an active platform admin (403)
--   MKA02 tenant or user not found (404)
--   MKA03 unknown plan (422)
--   MKA04 invalid argument, for example a status other than active or suspended (422)
--   MKA05 the default tenant cannot be suspended or deleted (409)
--   MKA06 an admin cannot suspend or delete a tenant it belongs to (409)
--   MKA07 a platform admin's password is managed by its Secret, not the panel (409)
--   MKA08 the tenant is deleted (409)

-- Tenants with their plan and usage, newest first, with the number of matches in every row
-- (total_count). p_query matches the slug, the name, the id or the login of a member (substring,
-- case-insensitive). Deleted tenants only with p_include_deleted. Counts: memberships, live http
-- monitors and live heartbeats (paused ones count, like the plan quotas).
-- +goose StatementBegin
CREATE FUNCTION admin_list_tenants(p_actor uuid, p_query text, p_include_deleted boolean, p_limit integer, p_offset integer)
RETURNS TABLE (
    id              uuid,
    slug            text,
    name            text,
    plan_key        text,
    plan_name       text,
    status          text,
    created_at      timestamptz,
    deleted_at      timestamptz,
    user_count      integer,
    monitor_count   integer,
    heartbeat_count integer,
    total_count     integer
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    q text := lower(btrim(COALESCE(p_query, '')));
BEGIN
    PERFORM admin_require_platform_admin(p_actor);
    RETURN QUERY
    WITH matched AS (
        SELECT t.id, t.slug, t.name, t.plan_key, t.status, t.created_at, t.deleted_at
        FROM tenants AS t
        WHERE (p_include_deleted OR t.deleted_at IS NULL)
          AND (q = ''
               OR strpos(lower(t.slug), q) > 0
               OR strpos(lower(t.name), q) > 0
               OR t.id::text = q
               OR EXISTS (SELECT 1 FROM memberships AS m JOIN users AS u ON u.id = m.user_id
                          WHERE m.tenant_id = t.id AND strpos(lower(u.email::text), q) > 0))
    ), page AS (
        SELECT mt.*, count(*) OVER () AS total
        FROM matched AS mt
        ORDER BY mt.created_at DESC, mt.id DESC
        LIMIT least(greatest(COALESCE(p_limit, 25), 1), 100)
        OFFSET greatest(COALESCE(p_offset, 0), 0)
    )
    SELECT p.id, p.slug, p.name, p.plan_key, pl.name, p.status, p.created_at, p.deleted_at,
           (SELECT count(*)::integer FROM memberships AS m WHERE m.tenant_id = p.id),
           (SELECT count(*)::integer FROM monitors AS mo WHERE mo.tenant_id = p.id AND mo.type = 'http' AND mo.deleted_at IS NULL),
           (SELECT count(*)::integer FROM monitors AS mo WHERE mo.tenant_id = p.id AND mo.type = 'heartbeat' AND mo.deleted_at IS NULL),
           p.total::integer
    FROM page AS p
    JOIN plans AS pl ON pl.key = p.plan_key
    ORDER BY p.created_at DESC, p.id DESC;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_list_tenants(uuid, text, boolean, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_list_tenants(uuid, text, boolean, integer, integer) TO maiak_api;

-- One tenant (deleted ones included) with the same columns as the list; MKA02 when unknown.
-- +goose StatementBegin
CREATE FUNCTION admin_get_tenant(p_actor uuid, p_tenant_id uuid)
RETURNS TABLE (
    id              uuid,
    slug            text,
    name            text,
    plan_key        text,
    plan_name       text,
    status          text,
    created_at      timestamptz,
    deleted_at      timestamptz,
    user_count      integer,
    monitor_count   integer,
    heartbeat_count integer
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
#variable_conflict use_column
BEGIN
    PERFORM admin_require_platform_admin(p_actor);
    RETURN QUERY
    SELECT t.id, t.slug, t.name, t.plan_key, pl.name, t.status, t.created_at, t.deleted_at,
           (SELECT count(*)::integer FROM memberships AS m WHERE m.tenant_id = t.id),
           (SELECT count(*)::integer FROM monitors AS mo WHERE mo.tenant_id = t.id AND mo.type = 'http' AND mo.deleted_at IS NULL),
           (SELECT count(*)::integer FROM monitors AS mo WHERE mo.tenant_id = t.id AND mo.type = 'heartbeat' AND mo.deleted_at IS NULL)
    FROM tenants AS t
    JOIN plans AS pl ON pl.key = t.plan_key
    WHERE t.id = p_tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'admin: tenant not found' USING ERRCODE = 'MKA02';
    END IF;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_get_tenant(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_get_tenant(uuid, uuid) TO maiak_api;

-- Moves a live tenant to another plan. Returns false when it already is on it (nothing written).
-- Locks the tenant row like plans.CheckCreate, so a plan change and a quota check of the same
-- tenant are serialised. Existing data above the new limits is kept; only new creations fail.
-- +goose StatementBegin
CREATE FUNCTION admin_set_tenant_plan(p_actor uuid, p_tenant_id uuid, p_plan_key text, p_audit_id uuid, p_ip inet)
RETURNS boolean
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_old     text;
    v_deleted timestamptz;
BEGIN
    PERFORM admin_require_platform_admin(p_actor);
    SELECT t.plan_key, t.deleted_at INTO v_old, v_deleted FROM tenants AS t WHERE t.id = p_tenant_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'admin: tenant not found' USING ERRCODE = 'MKA02';
    END IF;
    IF v_deleted IS NOT NULL THEN
        RAISE EXCEPTION 'admin: tenant is deleted' USING ERRCODE = 'MKA08';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM plans AS p WHERE p.key = p_plan_key) THEN
        RAISE EXCEPTION 'admin: unknown plan' USING ERRCODE = 'MKA03';
    END IF;
    IF v_old = p_plan_key THEN
        RETURN false;
    END IF;
    UPDATE tenants SET plan_key = p_plan_key, updated_at = now() WHERE id = p_tenant_id;
    PERFORM admin_audit(p_tenant_id, p_actor, p_audit_id, 'admin.tenant.plan_changed', 'tenant', p_tenant_id::text,
                        jsonb_build_object('from', v_old, 'to', p_plan_key), p_ip);
    RETURN true;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_set_tenant_plan(uuid, uuid, text, uuid, inet) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_set_tenant_plan(uuid, uuid, text, uuid, inet) TO maiak_api;

-- Suspends ('suspended') or unsuspends ('active') a live tenant. Returns false when the status
-- does not change. The default tenant and a tenant the actor belongs to are never suspended.
-- +goose StatementBegin
CREATE FUNCTION admin_set_tenant_status(p_actor uuid, p_tenant_id uuid, p_status text, p_audit_id uuid, p_ip inet)
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
    PERFORM admin_audit(p_tenant_id, p_actor, p_audit_id,
                        CASE WHEN p_status = 'suspended' THEN 'admin.tenant.suspended' ELSE 'admin.tenant.unsuspended' END,
                        'tenant', p_tenant_id::text, jsonb_build_object('from', v_old, 'to', p_status), p_ip);
    RETURN true;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_set_tenant_status(uuid, uuid, text, uuid, inet) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_set_tenant_status(uuid, uuid, text, uuid, inet) TO maiak_api;

-- Soft-deletes a tenant: deleted_at and status 'suspended' (so a previous app version, which
-- checks only the status at login, refuses it too). Its data stays; its sessions are refused by
-- auth_lookup_session, its monitors, heartbeats and deliveries are no longer claimed. Returns
-- false for an already deleted tenant. Never the default tenant or one the actor belongs to.
-- +goose StatementBegin
CREATE FUNCTION admin_delete_tenant(p_actor uuid, p_tenant_id uuid, p_audit_id uuid, p_ip inet)
RETURNS boolean
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_status  text;
    v_slug    text;
    v_plan    text;
    v_deleted timestamptz;
BEGIN
    PERFORM admin_require_platform_admin(p_actor);
    SELECT t.status, t.slug, t.plan_key, t.deleted_at INTO v_status, v_slug, v_plan, v_deleted
    FROM tenants AS t WHERE t.id = p_tenant_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'admin: tenant not found' USING ERRCODE = 'MKA02';
    END IF;
    IF v_deleted IS NOT NULL THEN
        RETURN false;
    END IF;
    IF v_slug = 'default' THEN
        RAISE EXCEPTION 'admin: the default tenant cannot be deleted' USING ERRCODE = 'MKA05';
    END IF;
    IF EXISTS (SELECT 1 FROM memberships AS m WHERE m.tenant_id = p_tenant_id AND m.user_id = p_actor) THEN
        RAISE EXCEPTION 'admin: cannot delete a tenant the admin belongs to' USING ERRCODE = 'MKA06';
    END IF;
    UPDATE tenants SET deleted_at = now(), status = 'suspended', updated_at = now() WHERE id = p_tenant_id;
    PERFORM admin_audit(p_tenant_id, p_actor, p_audit_id, 'admin.tenant.deleted', 'tenant', p_tenant_id::text,
                        jsonb_build_object('status', v_status, 'plan', v_plan), p_ip);
    RETURN true;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_delete_tenant(uuid, uuid, uuid, inet) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_delete_tenant(uuid, uuid, uuid, inet) TO maiak_api;

-- The members of a tenant (deleted tenants included), oldest membership first, with the number
-- of matches in every row. p_query matches the login (substring, case-insensitive).
-- +goose StatementBegin
CREATE FUNCTION admin_list_users(p_actor uuid, p_tenant_id uuid, p_query text, p_limit integer, p_offset integer)
RETURNS TABLE (
    id                uuid,
    login             text,
    status            text,
    created_at        timestamptz,
    is_platform_admin boolean,
    role              text,
    total_count       integer
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    q text := lower(btrim(COALESCE(p_query, '')));
BEGIN
    PERFORM admin_require_platform_admin(p_actor);
    IF NOT EXISTS (SELECT 1 FROM tenants AS t WHERE t.id = p_tenant_id) THEN
        RAISE EXCEPTION 'admin: tenant not found' USING ERRCODE = 'MKA02';
    END IF;
    RETURN QUERY
    SELECT u.id, u.email::text, u.status, u.created_at, u.is_platform_admin, m.role,
           (count(*) OVER ())::integer
    FROM memberships AS m
    JOIN users AS u ON u.id = m.user_id
    WHERE m.tenant_id = p_tenant_id
      AND (q = '' OR strpos(lower(u.email::text), q) > 0)
    ORDER BY m.created_at, u.id
    LIMIT least(greatest(COALESCE(p_limit, 25), 1), 100)
    OFFSET greatest(COALESCE(p_offset, 0), 0);
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_list_users(uuid, uuid, text, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_list_users(uuid, uuid, text, integer, integer) TO maiak_api;

-- Replaces a user's password hash (an argon2id PHC string computed by the api from a random
-- temporary password) and deletes all of the user's sessions in every tenant. Refuses platform
-- admins (MKA07: the system admin's credentials come from its Secret). The audit row goes to the
-- tenant the user logs into (live before deleted, default first, then the oldest membership);
-- a user without any membership is not reachable through the panel (MKA02). Returns that tenant
-- and the number of sessions deleted.
-- +goose StatementBegin
CREATE FUNCTION admin_reset_user_password(p_actor uuid, p_user_id uuid, p_password_hash text, p_audit_id uuid, p_ip inet)
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
    SELECT m.tenant_id INTO v_tenant
    FROM memberships AS m
    JOIN tenants AS t ON t.id = m.tenant_id
    WHERE m.user_id = p_user_id
    ORDER BY (t.deleted_at IS NULL) DESC, (t.slug = 'default') DESC, m.created_at, m.tenant_id
    LIMIT 1;
    IF v_tenant IS NULL THEN
        RAISE EXCEPTION 'admin: user has no membership' USING ERRCODE = 'MKA02';
    END IF;
    UPDATE users SET password_hash = p_password_hash, updated_at = now() WHERE id = p_user_id;
    v_n := auth_revoke_user_sessions(p_user_id);
    PERFORM admin_audit(v_tenant, p_actor, p_audit_id, 'admin.user.password_reset', 'user', p_user_id::text,
                        jsonb_build_object('sessions_revoked', v_n), p_ip);
    RETURN QUERY SELECT v_tenant, v_n;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_reset_user_password(uuid, uuid, text, uuid, inet) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_reset_user_password(uuid, uuid, text, uuid, inet) TO maiak_api;

-- Platform-wide counts as one JSON object: tenants by state (deleted ones only in "deleted"),
-- active users, live http monitors and heartbeats of tenants that are not deleted, and every plan
-- with its number of tenants that are not deleted.
-- +goose StatementBegin
CREATE FUNCTION admin_stats(p_actor uuid)
RETURNS jsonb
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    PERFORM admin_require_platform_admin(p_actor);
    RETURN jsonb_build_object(
        'tenants', (SELECT jsonb_build_object(
                        'total', count(*) FILTER (WHERE t.deleted_at IS NULL),
                        'active', count(*) FILTER (WHERE t.deleted_at IS NULL AND t.status = 'active'),
                        'suspended', count(*) FILTER (WHERE t.deleted_at IS NULL AND t.status = 'suspended'),
                        'deleted', count(*) FILTER (WHERE t.deleted_at IS NOT NULL))
                    FROM tenants AS t),
        'users', (SELECT count(*) FROM users AS u WHERE u.status = 'active'),
        'monitors', (SELECT count(*) FROM monitors AS m JOIN tenants AS t ON t.id = m.tenant_id
                     WHERE m.type = 'http' AND m.deleted_at IS NULL AND t.deleted_at IS NULL),
        'heartbeats', (SELECT count(*) FROM monitors AS m JOIN tenants AS t ON t.id = m.tenant_id
                       WHERE m.type = 'heartbeat' AND m.deleted_at IS NULL AND t.deleted_at IS NULL),
        'plans', (SELECT COALESCE(jsonb_agg(jsonb_build_object(
                         'key', p.key, 'name', p.name,
                         'tenants', (SELECT count(*) FROM tenants AS t WHERE t.plan_key = p.key AND t.deleted_at IS NULL))
                      ORDER BY p.key), '[]'::jsonb)
                  FROM plans AS p)
    );
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION admin_stats(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_stats(uuid) TO maiak_api;

-- Same signature, new body: a user whose only memberships are in deleted tenants now gets such a
-- tenant (live ones still win), so the login can answer "account suspended" after a correct
-- password instead of the generic failure. The previous app version reads the tenant with
-- tenants.Get, which skips deleted tenants; it fails such a login (no data is exposed).
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
    ORDER BY (t.deleted_at IS NULL) DESC, (t.slug = 'default') DESC, m.created_at, m.tenant_id
    LIMIT 1
$$;
-- +goose StatementEnd

-- Same signature and result, new body: deliveries of suspended or deleted tenants are not
-- claimed (they stay pending until the tenant is active again); everything else is unchanged.
-- tenants has no RLS, the owner reads it directly.
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
