-- +goose Up
-- Allow the cross-tool governed 'rule' artifact type (Phase 3 Slice B).
ALTER TABLE artifact DROP CONSTRAINT artifact_type_check;
ALTER TABLE artifact ADD CONSTRAINT artifact_type_check CHECK (type IN ('skill', 'subagent', 'rule'));

-- +goose Down
ALTER TABLE artifact DROP CONSTRAINT artifact_type_check;
ALTER TABLE artifact ADD CONSTRAINT artifact_type_check CHECK (type IN ('skill', 'subagent'));
