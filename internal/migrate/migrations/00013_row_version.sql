-- +goose Up
-- Optimistic-concurrency token for the two entities with full-replace updates.
--
-- Maintained by a trigger, NOT by hand in each UPDATE. `updated_at` is already
-- hand-maintained in six statements (artifact.go:280,324,335,350,368 and
-- artifact_revision.go:209); a seventh that forgot would silently stop the
-- value changing, and a version that stops changing does not fail loudly — it
-- ACCEPTS STALE WRITES. The trigger cannot be forgotten by a future statement.
ALTER TABLE mcp_server ADD COLUMN row_version bigint NOT NULL DEFAULT 1;
ALTER TABLE artifact   ADD COLUMN row_version bigint NOT NULL DEFAULT 1;

-- +goose StatementBegin
CREATE FUNCTION bump_row_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.row_version := OLD.row_version + 1;
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- If a future flow ever issues two UPDATEs on the same row within one
-- transaction, the client-facing ETag must come from the LAST write's
-- RETURNING — the first UPDATE's RETURNING is stale by commit, since the
-- trigger bumps row_version again on the second UPDATE. Not a concern for any
-- current flow: SetArtifactApproved and RollbackArtifact each issue exactly
-- one UPDATE on artifact (their second write is an INSERT into
-- artifact_revision).
CREATE TRIGGER mcp_server_bump_row_version BEFORE UPDATE ON mcp_server
  FOR EACH ROW EXECUTE FUNCTION bump_row_version();
CREATE TRIGGER artifact_bump_row_version BEFORE UPDATE ON artifact
  FOR EACH ROW EXECUTE FUNCTION bump_row_version();

-- +goose Down
-- Both triggers must go BEFORE the shared function, or the DROP fails on the
-- dependency (round-trip covered by TestRowVersionDownUpRoundTrip).
--
-- This does NOT — and cannot — restore each row's row_version to what it was
-- pre-Up; the column is dropped and recreated at DEFAULT 1, so every row
-- resets to 1 (mirroring 00009's status normalization, which is lossless for
-- visibility but not for the original distinct values). Because 1 is the
-- modal value of a version counter, a client still holding a pre-rollback
-- `If-Match: "1"` would match again post-rollback, and its stale write would
-- be accepted — the guard silently reopens for exactly the value most likely
-- to be cached. Operator-initiated (a manual down/up) and rare, but recorded
-- rather than passed over.
DROP TRIGGER IF EXISTS artifact_bump_row_version ON artifact;
DROP TRIGGER IF EXISTS mcp_server_bump_row_version ON mcp_server;
DROP FUNCTION IF EXISTS bump_row_version();
ALTER TABLE artifact   DROP COLUMN IF EXISTS row_version;
ALTER TABLE mcp_server DROP COLUMN IF EXISTS row_version;
