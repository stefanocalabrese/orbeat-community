-- +goose Up
-- Mandatory acknowledgment of scan findings, author side (data model only;
-- docs/plans/orbeat-scan-acknowledgment-2026-08-27.md). scan_findings already
-- persists per artifact (00006's scan_findings jsonb); this adds the DIGEST of
-- those findings computed at submit, and the author's acknowledgment of a
-- digest. The scanner is nondeterministic and a withdraw-then-resubmit
-- re-scans, so a plain boolean "acknowledged" flag would silently survive a
-- re-scan and end up describing findings the author never read. Binding the
-- acknowledgment to the digest it was read against is what makes it invalid
-- the instant a re-scan changes the findings.
--
-- All four columns are nullable and none is backfilled, so every existing row
-- reads correctly as "no digest recorded, not acknowledged" with no backfill
-- step: scan_findings on a pre-existing row already reflects whatever a prior
-- submit stored (or 00006's '[]' default), and nothing here needs to
-- reconstruct a digest for a scan that already ran before this column
-- existed. Same additive shape 00026 and 00027 used for their own columns.
ALTER TABLE artifact
    ADD COLUMN scan_findings_digest text,
    ADD COLUMN findings_ack_digest  text,
    ADD COLUMN findings_ack_by      text,
    ADD COLUMN findings_ack_at      timestamptz;

-- The three acknowledgment columns are written together or not at all,
-- mirroring 00016's artifact_approved_identity_complete: a partial write
-- would let a row carry a "who" or "when" with no digest saying what was
-- acknowledged, or a digest with nobody recorded as having read it.
-- scan_findings_digest is deliberately NOT part of this CHECK: a digest can
-- exist with no acknowledgment yet (the ordinary "submitted, not yet
-- acknowledged" state), and an acknowledgment can exist for a digest that no
-- longer matches the current scan_findings_digest (a stale acknowledgment
-- left behind by a re-scan) -- both are states this feature exists to tell
-- apart, not integrity violations to reject.
ALTER TABLE artifact ADD CONSTRAINT artifact_findings_ack_complete CHECK (
        (findings_ack_digest IS NULL) = (findings_ack_by IS NULL)
    AND (findings_ack_digest IS NULL) = (findings_ack_at IS NULL));

-- +goose Down
ALTER TABLE artifact DROP CONSTRAINT artifact_findings_ack_complete;
ALTER TABLE artifact
    DROP COLUMN scan_findings_digest,
    DROP COLUMN findings_ack_digest,
    DROP COLUMN findings_ack_by,
    DROP COLUMN findings_ack_at;
