-- +goose Up
-- Closes audit B30: ON DELETE SET NULL on virtual_key.created_by fires the
-- shared bump_row_version() trigger, bumping row_version and causing spurious
-- 412 Precondition Failed for admins holding valid ETags.
--
-- Replaces the trigger with a custom function that skips bumping when only
-- created_by changes from non-NULL to NULL (the automatic cascade path).
DROP TRIGGER IF EXISTS virtual_key_bump_row_version ON virtual_key;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION virtual_key_safe_bump_row_version()
RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Skip bumping when only created_by changes from non-NULL to NULL.
    -- This is the ON DELETE SET NULL cascade from user deletion; bumping
    -- row_version here causes spurious 412s for admins holding valid ETags.
    IF OLD.created_by IS NOT NULL AND NEW.created_by IS NULL
       AND OLD.client_id = NEW.client_id
       AND OLD.role_id = NEW.role_id
       AND OLD.name = NEW.name
       AND OLD.description = NEW.description
       AND OLD.allowed_tools IS NOT DISTINCT FROM NEW.allowed_tools
       AND OLD.registration_access_token_ref = NEW.registration_access_token_ref
       AND OLD.revoked_at IS NOT DISTINCT FROM NEW.revoked_at
       AND OLD.created_at = NEW.created_at
    THEN
        RETURN NEW;
    END IF;

    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER virtual_key_bump_row_version BEFORE UPDATE ON virtual_key
    FOR EACH ROW EXECUTE FUNCTION virtual_key_safe_bump_row_version();

-- +goose Down
DROP TRIGGER IF EXISTS virtual_key_bump_row_version ON virtual_key;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION virtual_key_safe_bump_row_version()
RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER virtual_key_bump_row_version BEFORE UPDATE ON virtual_key
    FOR EACH ROW EXECUTE FUNCTION virtual_key_safe_bump_row_version();

DROP TABLE virtual_key;
