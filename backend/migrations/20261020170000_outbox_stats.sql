-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Outbox backlog for the jobs metric maiak_jobs_outbox_pending (ADR-0051). The count spans
-- tenants, so it is a SECURITY DEFINER function (ADR-0016 pattern): owned by maiak_owner,
-- search_path pinned, EXECUTE only for maiak_worker, no arguments. It reads through the existing
-- owner_delivery_read policy and returns only a number. Rows waiting for a retry count as
-- pending. The partial index notification_deliveries_ready_idx keeps it cheap.
-- +goose StatementBegin
CREATE FUNCTION outbox_pending()
RETURNS bigint
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT count(*) FROM notification_deliveries WHERE status = 'pending'
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION outbox_pending() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION outbox_pending() TO maiak_worker;
