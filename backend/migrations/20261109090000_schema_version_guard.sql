-- +goose Up
SET LOCAL lock_timeout = '5s';

-- O-06: api, worker and jobs read the newest applied migration at start and refuse to run when
-- the schema is newer than the binary (migrations.CheckNotAhead, closes L-282). The version table
-- holds no tenant data and no secrets; SELECT is all they get. The status page renderer has no
-- table privileges at all and does not check.
GRANT SELECT ON goose_db_version TO maiak_api, maiak_worker;
