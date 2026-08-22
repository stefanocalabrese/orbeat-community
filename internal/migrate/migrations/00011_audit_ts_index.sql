-- +goose Up
-- The audit-retention prune filters on `ts < cutoff` with NO tenant_id predicate,
-- so it cannot use audit_event_tenant_ts_id_idx (leading column tenant_id). A
-- plain ts B-tree turns each prune batch into an O(batch) index range scan
-- instead of a sequential scan that degrades as the table grows.
CREATE INDEX audit_event_ts_idx ON audit_event (ts);

-- +goose Down
DROP INDEX audit_event_ts_idx;
