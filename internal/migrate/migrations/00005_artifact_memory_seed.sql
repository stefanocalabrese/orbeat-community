-- +goose Up
-- Governed seed memory body for a subagent (Slice B). Structural invariant
-- (subagent-only) enforced here, mirroring 00003's memory_scope CHECK; the
-- delivery *policy* (memory_scope must be user/project) stays in Go at the
-- admin/sync layers, since it may evolve (org seeding is future work).
ALTER TABLE artifact ADD COLUMN memory_seed text
    CHECK (memory_seed IS NULL OR type = 'subagent');

-- +goose Down
ALTER TABLE artifact DROP COLUMN memory_seed;
