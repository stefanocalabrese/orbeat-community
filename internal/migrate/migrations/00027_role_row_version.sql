-- +goose Up
-- Optimistic concurrency for role edits.
--
-- The role-rename slice adds `store.UpdateRoleName`, a full-replace update of
-- a role's name. Renaming a role while another admin is mid-edit would
-- otherwise silently last-write-win, exactly the defect migration 00013 and
-- 00026 already closed on mcp_server, artifact and entitlement.
--
-- Same shape as 00026 deliberately: a BEFORE UPDATE trigger rather than
-- hand-maintained SQL, because a statement that forgets to bump would silently
-- ACCEPT a stale write, which is the failure this exists to prevent.
ALTER TABLE role ADD COLUMN row_version bigint NOT NULL DEFAULT 1;

-- +goose StatementBegin
CREATE TRIGGER role_bump_row_version BEFORE UPDATE ON role
    FOR EACH ROW EXECUTE FUNCTION bump_row_version();
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER role_bump_row_version ON role;
ALTER TABLE role DROP COLUMN row_version;
