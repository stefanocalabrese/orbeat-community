-- +goose Up
-- Optimistic concurrency for entitlement edits.
--
-- `PUT /v1/admin/entitlements/{id}` is a FULL-REPLACE update of allowedTools and
-- permissions, which is exactly the shape migration 00013 added row_version for
-- on mcp_server and artifact: two admins editing the same entitlement would
-- otherwise silently last-write-wins, and the value being overwritten is the
-- list of tools a role may call. Adding this route without the column would
-- reintroduce the defect that slice closed, on a narrower but more sensitive
-- surface.
--
-- Same shape as 00013 deliberately: a BEFORE UPDATE trigger rather than
-- hand-maintained SQL, because a statement that forgets to bump would silently
-- ACCEPT a stale write, which is the failure this exists to prevent.
ALTER TABLE entitlement ADD COLUMN row_version bigint NOT NULL DEFAULT 1;

-- +goose StatementBegin
CREATE TRIGGER entitlement_bump_row_version BEFORE UPDATE ON entitlement
    FOR EACH ROW EXECUTE FUNCTION bump_row_version();
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER entitlement_bump_row_version ON entitlement;
ALTER TABLE entitlement DROP COLUMN row_version;
