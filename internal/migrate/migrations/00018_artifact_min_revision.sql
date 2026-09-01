-- +goose Up
-- The admin's minimum-revision FLOOR (docs/specs/2026-08-22-orbeat-artifact-
-- version-pinning-design.md sec 8): the oldest revision an admin will let any
-- developer machine keep serving for this artifact. A developer pins a version
-- in her own sync config; this column is the one override that outranks every
-- such pin, so a security fix cannot be sat out indefinitely.
--
-- 0 MEANS NO FLOOR, and it is unambiguous because insertRevision numbers from
-- 1 (internal/store/artifact_revision.go). It matches the off-sentinel
-- convention editionLimits already records for revisionKeep and
-- auditExportMaxRows (internal/api/editionlimits.go).
--
-- NOT NULL DEFAULT 0 rather than nullable, so every existing row is unaffected
-- on upgrade and no reader needs a null branch.
--
-- NO FOREIGN KEY TO artifact_revision, and it is not an omission. There cannot
-- be one: the revision a floor names can be pruned out from under it
-- (ORBEAT_ARTIFACT_REVISION_KEEP). artifact_revision.restored_from_num is the
-- precedent, a plain int reference into the same chain for the same reason
-- (00007_artifact_revision.sql:13). A floor pointing below the oldest
-- surviving revision is harmless: the clamp resolves against the window that
-- actually exists (spec sec 4.2), so a dangling floor serves the oldest
-- survivor rather than failing.
--
-- The CHECK is NAMED so a test can assert which constraint fired rather than
-- just that some integrity constraint did (store's isConstraintViolation
-- helper matches on ConstraintName), following 00016's
-- artifact_approved_identity_complete and 00017's
-- artifact_deployment_revision_positive.
--
-- row_version needs no maintenance here. The artifact_bump_row_version trigger
-- from 00013_row_version.sql:29 fires BEFORE UPDATE ON artifact, so setting a
-- floor invalidates outstanding client ETags with no new machinery.
ALTER TABLE artifact ADD COLUMN min_revision_num int NOT NULL DEFAULT 0
    CONSTRAINT artifact_min_revision_num_non_negative CHECK (min_revision_num >= 0);

-- +goose Down
ALTER TABLE artifact DROP COLUMN min_revision_num;
