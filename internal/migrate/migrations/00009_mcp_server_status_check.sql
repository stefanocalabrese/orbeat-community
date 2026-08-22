-- +goose Up
-- Normalize before constraining. This is LOSSLESS FOR VISIBILITY ONLY: today's
-- behavior is literally "anything != 'active' is hidden from catalog+gateway",
-- which is exactly what 'disabled' now means — so no server changes
-- visibility. It is NOT lossless for the stored value itself: distinct legacy
-- values (e.g. 'inactive', 'maintenance', '') are collapsed into 'disabled'
-- and cannot be told apart again afterward.
UPDATE mcp_server SET status = 'disabled' WHERE status <> 'active';
ALTER TABLE mcp_server ADD CONSTRAINT mcp_server_status_check CHECK (status IN ('active','disabled'));

-- +goose Down
-- Only drops the constraint. It does NOT — and cannot — restore the original
-- distinct values the Up normalization collapsed into 'disabled'; those are
-- gone for good. Down just stops enforcing the allowed set going forward.
ALTER TABLE mcp_server DROP CONSTRAINT mcp_server_status_check;
