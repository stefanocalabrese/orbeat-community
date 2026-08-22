-- +goose Up
-- Phase 4 artifact approval governance: approval workflow + approved snapshot.
ALTER TABLE artifact
    ADD COLUMN approval_state text NOT NULL DEFAULT 'draft'
        CHECK (approval_state IN ('draft','pending','approved','rejected')),
    ADD COLUMN approved_content text,
    ADD COLUMN approved_memory_seed text,
    ADD COLUMN approved_memory_scope text,
    ADD COLUMN submitted_by text,
    ADD COLUMN submitted_at timestamptz,
    ADD COLUMN approved_by text,
    ADD COLUMN approved_at timestamptz,
    ADD COLUMN reject_reason text,
    ADD COLUMN scan_findings jsonb NOT NULL DEFAULT '[]'::jsonb;

-- Grandfather live artifacts: status='active' becomes approved with the current
-- content frozen as the snapshot, so nothing drops out of distribution on upgrade.
UPDATE artifact SET
    approval_state = 'approved',
    approved_content = content,
    approved_memory_seed = memory_seed,
    approved_memory_scope = memory_scope,
    approved_by = 'system:migration',
    approved_at = now()
WHERE status = 'active';
-- status='draft' rows keep approval_state='draft' (the column default), no snapshot.

ALTER TABLE artifact DROP COLUMN status;

-- +goose Down
ALTER TABLE artifact ADD COLUMN status text NOT NULL DEFAULT 'draft'
    CHECK (status IN ('draft','active'));
UPDATE artifact SET status = 'active' WHERE approved_content IS NOT NULL;
ALTER TABLE artifact
    DROP COLUMN approval_state, DROP COLUMN approved_content,
    DROP COLUMN approved_memory_seed, DROP COLUMN approved_memory_scope,
    DROP COLUMN submitted_by, DROP COLUMN submitted_at,
    DROP COLUMN approved_by, DROP COLUMN approved_at,
    DROP COLUMN reject_reason, DROP COLUMN scan_findings;
