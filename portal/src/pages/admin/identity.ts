import type { AdminArtifact, ArtifactRevision } from "../../api/types";

/**
 * An artifact's identity is its type, name and visibility, in the order the
 * API reports them. Together they decide the file path an artifact lands on
 * (`skills/<name>/SKILL.md`, `agents/<name>.md`) and the channel it arrives
 * on, so a change to any of the three moves or re-routes a file on every
 * machine that receives the artifact.
 */
export const IDENTITY_FIELDS = ["type", "name", "visibility"] as const;

export type IdentityField = (typeof IDENTITY_FIELDS)[number];

/** One field whose distributed value and working value have come apart. */
export interface IdentityChange {
  field: IdentityField;
  /** What developers receive today, i.e. the approved snapshot's value. */
  from: string;
  /** What the working copy says, i.e. what distribution becomes on approval. */
  to: string;
}

/**
 * The identity edits that are saved but not yet distributed.
 *
 * Migration 00016 snapshots type/name/visibility at approval the way content
 * already was, so editing one of them leaves the artifact's approved snapshot
 * serving the old identity until a second admin approves the change. That gap
 * is invisible on every screen that renders the live row, and it is the whole
 * answer to "why is the file on my machine still called foo".
 *
 * Keyed on the three `approved*` fields being PRESENT, not on `approved`. The
 * server omits all four approved fields together when nothing is distributed
 * (the `artifact_approved_identity_complete` CHECK makes that an invariant
 * rather than a convention, and `omitempty` on the Go side turns it into
 * absence on the wire), so an artifact with no snapshot yields no changes
 * without needing a second term to say so. An absent field must never be
 * compared: `undefined !== "bar"` is true, and the marker would then announce
 * a distributed identity that does not exist.
 *
 * Returns [] in a Community build for a different reason worth knowing: there
 * artifacts are auto-approved on write, so the working copy and the snapshot
 * commit in the same transaction and never diverge. Every indicator built on
 * this function is therefore silent there, which is correct rather than a
 * degraded experience.
 */
export function pendingIdentity(a: AdminArtifact): IdentityChange[] {
  const changes: IdentityChange[] = [];
  if (a.approvedType !== undefined && a.approvedType !== a.type) {
    changes.push({ field: "type", from: a.approvedType, to: a.type });
  }
  if (a.approvedName !== undefined && a.approvedName !== a.name) {
    changes.push({ field: "name", from: a.approvedName, to: a.name });
  }
  if (a.approvedVisibility !== undefined && a.approvedVisibility !== a.visibility) {
    changes.push({ field: "visibility", from: a.approvedVisibility, to: a.visibility });
  }
  return changes;
}

/** "type skill, name foo": the distributed side of a set of changes. */
export function distributedSummary(changes: IdentityChange[]): string {
  return changes.map((c) => `${c.field} ${c.from}`).join(", ");
}

export interface DistributedIdentity {
  type: string;
  name: string;
  visibility: string;
}

/**
 * What this artifact is distributed as right now.
 *
 * The `?? live` fallback is not a convenience: it mirrors, field for field,
 * the `COALESCE(approved_type, type)` fallback store.RollbackArtifact reads
 * when it needs an identity to keep. That case is real rather than
 * hypothetical, because rollback also runs on a WITHDRAWN artifact, whose
 * approved identity the CHECK cleared along with its content. Diverging from
 * the server's fallback here would make the rollback confirmation describe an
 * outcome the rollback does not produce.
 */
export function distributedIdentity(a: AdminArtifact): DistributedIdentity {
  return {
    type: a.approvedType ?? a.type,
    name: a.approvedName ?? a.name,
    visibility: a.approvedVisibility ?? a.visibility,
  };
}

function identityLine(i: DistributedIdentity): string {
  return `type ${i.type}, name ${i.name}, visibility ${i.visibility}`;
}

/**
 * The confirmation text for rolling distribution back to `r`.
 *
 * A revision is the complete approved state, identity included, so a rollback
 * can RENAME the artifact for everyone receiving it. That is the accepted cost
 * of the design and it is only acceptable if the admin sees it before
 * confirming.
 *
 * The silent case is the one that needs the most words. A revision approved
 * before migration 00016 recorded no identity, and 00016 deliberately did not
 * backfill one into an append-only table, so rolling back to it restores the
 * content and leaves the distributed identity exactly where it is. Saying
 * nothing there would read as "no rename", which happens to be true, and would
 * read identically if the field were simply missing from the response.
 */
export function rollbackConfirmMessage(r: ArtifactRevision, current: DistributedIdentity): string {
  const head = `Roll distribution back to revision #${r.revision}?`;
  if (r.type === undefined || r.name === undefined || r.visibility === undefined) {
    return `${head}\n\nRevision #${r.revision} recorded no identity, so only the content reverts. It keeps being distributed as ${identityLine(current)}.`;
  }
  const target: DistributedIdentity = { type: r.type, name: r.name, visibility: r.visibility };
  const moves = IDENTITY_FIELDS.filter((f) => target[f] !== current[f]).map(
    (f) => `${f} ${current[f]} → ${target[f]}`,
  );
  if (moves.length === 0) {
    return `${head}\n\nThe content reverts. It keeps being distributed as ${identityLine(current)}.`;
  }
  return `${head}\n\nThis also changes what every machine receiving it gets: ${moves.join(", ")}.`;
}
