-- name: InsertResult :exec
INSERT INTO check_results (tenant_id, monitor_id, location_id, checked_at, ok, status_code, latency_ms, error_kind, tls_not_after)
VALUES (@tenant_id, @monitor_id, @location_id, @checked_at, @ok, @status_code, @latency_ms, @error_kind, @tls_not_after);
