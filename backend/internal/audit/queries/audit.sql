-- name: InsertAuditEntry :exec
INSERT INTO audit_log (id, tenant_id, actor_user_id, action, entity_type, entity_id, metadata, ip)
VALUES (@id, @tenant_id, @actor_user_id, @action, @entity_type, @entity_id, @metadata, @ip);
