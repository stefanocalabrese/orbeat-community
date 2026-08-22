-- +goose Up
-- Phase 4+ artifact revision history: append-only chain of every approved
-- version, feeding one-click rollback. artifact.approved_* stays the live snapshot.
CREATE TABLE artifact_revision (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    artifact_id       uuid NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    revision_num      int  NOT NULL,
    content           text NOT NULL,
    memory_seed       text,
    memory_scope      text,
    source            text NOT NULL CHECK (source IN ('approval','rollback')),
    restored_from_num int,
    approved_by       text NOT NULL,
    approved_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (artifact_id, revision_num),
    CHECK ((source = 'rollback') = (restored_from_num IS NOT NULL))
);

-- Grandfather every currently-approved artifact as revision 1 so the feature has
-- a v1 for pre-existing rows and the "approved_content == latest revision" invariant
-- holds immediately after this migration.
INSERT INTO artifact_revision
    (tenant_id, artifact_id, revision_num, content, memory_seed, memory_scope, source, restored_from_num, approved_by, approved_at)
SELECT tenant_id, id, 1, approved_content, approved_memory_seed, approved_memory_scope,
       'approval', NULL, COALESCE(NULLIF(approved_by,''), 'system:migration'), COALESCE(approved_at, now())
FROM artifact
WHERE approved_content IS NOT NULL;

-- +goose Down
DROP TABLE artifact_revision;
