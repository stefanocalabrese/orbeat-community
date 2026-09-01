-- +goose Up
-- Artifact identity goes through approval (docs/specs/2026-08-22-orbeat-
-- artifact-identity-approval-design.md sec 3). An artifact's name and type are
-- the file path on every developer machine (skills/<name>/SKILL.md,
-- agents/<name>.md) and its visibility picks the channel, yet distribution
-- reads all three from the live row while content is served from a frozen
-- snapshot. Content has two copies and identity has one. These columns give
-- identity the second copy, the way 00006 gave it to content.

-- 1. The snapshot columns on artifact. Nullable, mirroring approved_content's
-- convention from 00006: NULL means "no approved snapshot exists". No value
-- CHECK on them: they are copies of columns the live CHECKs already constrain
-- (00003's type, 00004's visibility), and a second copy of
-- type IN ('skill','subagent','rule') is one more place to forget when a
-- fourth type lands.
ALTER TABLE artifact
    ADD COLUMN approved_type       text,
    ADD COLUMN approved_name       text,
    ADD COLUMN approved_visibility text;

-- 2. The same three on artifact_revision, so a revision is the COMPLETE
-- approved state rather than the approved payload plus whatever the name
-- happens to be now. Without them a rollback restores old content under the
-- current name, which is the desync this slice exists to prevent, recreated
-- by the feature meant to undo it.
ALTER TABLE artifact_revision
    ADD COLUMN type       text,
    ADD COLUMN name       text,
    ADD COLUMN visibility text;

-- 3. Backfill artifact, and artifact ONLY. Every already-approved row is
-- distributing under its live identity right now, so copying it across is a
-- no-op for every consumer: no artifact changes name, channel or path on
-- upgrade.
--
-- artifact_revision is deliberately NOT backfilled, because the sentence
-- above is false there. artifact_revision has never recorded what an artifact
-- was called at revision 3, only what it is called today, so writing today's
-- name into an old revision would put an unverified claim into an append-only
-- governance record. NULL there means "approved before 00016, identity not
-- recorded", and rollback reads it as "restore the content, leave the
-- approved identity where it is". The case is self-clearing: every approval
-- from here on writes the real values.
UPDATE artifact
   SET approved_type       = type,
       approved_name       = name,
       approved_visibility = visibility
 WHERE approved_content IS NOT NULL;

-- 4. Tie the identity to the snapshot. Load-bearing, not tidiness. The unique
-- index in step 5 is partial on approved_content IS NOT NULL, and a btree
-- treats NULLs as distinct, so a row carrying a snapshot with a NULL
-- approved_name would sit inside that index conflicting with nothing and
-- would distribute under an empty name. Three statements write the snapshot
-- (SetArtifactApproved, WithdrawArtifact, RollbackArtifact), which is three
-- chances to forget one column: the same argument 00013 used to pick a
-- trigger over hand-maintained SQL. This turns each of them into a constraint
-- violation in the first test that runs it.
--
-- approved_memory_scope and approved_memory_seed are deliberately NOT tied
-- in: both are legitimately NULL on an approved artifact.
ALTER TABLE artifact ADD CONSTRAINT artifact_approved_identity_complete CHECK (
        (approved_content IS NULL) = (approved_name IS NULL)
    AND (approved_content IS NULL) = (approved_type IS NULL)
    AND (approved_content IS NULL) = (approved_visibility IS NULL));

-- 5. The second namespace. 00003's UNIQUE (tenant_id, type, name) is on the
-- LIVE columns, and once distribution keys on the approved identity the live
-- constraint stops protecting the namespace that reaches disk: rename A from
-- foo to bar (A is draft now, still distributing as foo), create B as foo
-- (the live constraint allows it, A's live name is bar), approve B, and two
-- artifacts distribute as foo. That is not a visible failure.
-- RenderArtifactsPlugin builds a map[string]string keyed by path, so the
-- second row silently overwrites the first and the published tree holds one
-- fewer file than there are artifacts.
--
-- OPERATOR NOTE: this CREATE INDEX is where an upgrade fails with 23505 if
-- the database already holds two rows distributing under one identity. That
-- cannot arise through the API: 00003's UNIQUE (tenant_id, type, name) has
-- held since Phase 1 and step 3 copies from those exact columns. It can only
-- come from SQL applied directly to a production database. Resolve it by
-- withdrawing or renaming one of the two rows, then re-run the migration.
CREATE UNIQUE INDEX artifact_tenant_approved_identity_uniq
    ON artifact (tenant_id, approved_type, approved_name)
    WHERE approved_content IS NOT NULL;

-- 6. 00010 created artifact_tenant_distributable_idx on (tenant_id,
-- visibility) to "cover exactly the distributable set". This slice moves both
-- distribution filters to approved_visibility (spec sec 4), which would
-- orphan it: still maintained on every write, serving nothing. That is the
-- shape of the ORDER BY output-label defect from v1.22.0, which shipped inert
-- in audit.go from Phase 1 and was invisible because results were never
-- wrong. Index definitions live in migrations, so it is replaced here, in the
-- migration that adds the column it now keys on.
DROP INDEX artifact_tenant_distributable_idx;
CREATE INDEX artifact_tenant_distributable_idx
    ON artifact (tenant_id, approved_visibility)
    WHERE approved_content IS NOT NULL;

-- +goose Down
-- Restores 00010's definition of artifact_tenant_distributable_idx. 00010's
-- own Down drops that index name, so migrating below 00010 still ends with it
-- gone rather than with a duplicate.
DROP INDEX artifact_tenant_distributable_idx;
CREATE INDEX artifact_tenant_distributable_idx
    ON artifact (tenant_id, visibility)
    WHERE approved_content IS NOT NULL;

DROP INDEX artifact_tenant_approved_identity_uniq;
ALTER TABLE artifact DROP CONSTRAINT artifact_approved_identity_complete;
ALTER TABLE artifact_revision
    DROP COLUMN type, DROP COLUMN name, DROP COLUMN visibility;
ALTER TABLE artifact
    DROP COLUMN approved_type, DROP COLUMN approved_name, DROP COLUMN approved_visibility;
