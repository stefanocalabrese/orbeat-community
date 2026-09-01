import { ApiRequestError } from "../../api/client";

/**
 * The machine-readable `code` POST .../approve returns when the artifact
 * carries scan findings and its SUBMITTER has not acknowledged the CURRENT
 * digest (internal/api/precondition.go's authorFindingsAckRequiredCode,
 * docs/plans/orbeat-scan-acknowledgment-2026-08-27.md).
 */
export const AUTHOR_FINDINGS_ACK_REQUIRED_CODE = "author_findings_ack_required";

/**
 * The machine-readable `code` POST .../approve returns when the request
 * itself does not carry the APPROVER's own acknowledgment of the artifact's
 * CURRENT findings digest (internal/api/precondition.go's
 * approverFindingsAckRequiredCode).
 */
export const APPROVER_FINDINGS_ACK_REQUIRED_CODE = "approver_findings_ack_required";

/**
 * True iff `e` is either findings-acknowledgment refusal from POST
 * .../approve.
 *
 * ReviewQueuePage's own gating -- Approve stays disabled until the
 * artifact's own `findingsAcknowledged` field is true AND the approver has
 * ticked their own checkbox -- means the portal should never actually SEND a
 * request that gets refused this way. Reaching either code therefore means
 * the artifact's findings state moved between the review queue's last fetch
 * and the click: a withdraw/edit/resubmit produced a new digest, or another
 * reviewer's concurrent action changed what "current" means. That is exactly
 * as stale as a 412 row-version mismatch, so ReviewQueuePage treats both
 * identically -- ConflictNotice, reload to see the current state, never a
 * silent retry -- rather than drawing a second, narrower error UI for a case
 * the disabled button already tries to prevent.
 */
export function isFindingsAckRequired(e: unknown): e is ApiRequestError {
  return (
    e instanceof ApiRequestError &&
    (e.code === AUTHOR_FINDINGS_ACK_REQUIRED_CODE || e.code === APPROVER_FINDINGS_ACK_REQUIRED_CODE)
  );
}
