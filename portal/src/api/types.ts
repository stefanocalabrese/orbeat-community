export interface CatalogServer {
  id: string;
  name: string;
  description: string;
  transport: string;
  version: string;
  protocolVersion: string;
  status: string;
  allowedTools: string[] | null; // null = all tools
}

export interface AdminServer {
  id: string;
  name: string;
  description: string;
  transport: string;
  endpointOrCommand: string;
  version: string;
  protocolVersion: string;
  status: string;
  hasSecret: boolean;
  hasTlsCa: boolean;
  /**
   * The optimistic-concurrency token (spec §4). Carried on every server
   * response — list rows included, per `mcpServerCols`/`toAdminServerDTO` on
   * the Go side — so a list-row edit can echo it back as `If-Match` without
   * a redundant by-id fetch. Required, not optional: under `strict` +
   * `noUncheckedIndexedAccess`, a missing field is a compile error here
   * rather than an `undefined` that would render as `If-Match: "undefined"`
   * (a guaranteed 400).
   */
  rowVersion: number;
}

export interface ServerInput {
  name: string;
  description: string;
  transport: string;
  endpointOrCommand: string;
  version: string;
  protocolVersion: string;
  secretRef: string;
  tlsCaRef: string;
  status: string;
}

/**
 * Wire body for PUT /v1/admin/servers/{id} (defect 1, 2026-09-01, BREAKING).
 * Identical to ServerInput except secretRef/tlsCaRef are `?: string`, not
 * `string`: `JSON.stringify` drops an `undefined` property entirely, so
 * leaving one of these `undefined` OMITS the key from the request body,
 * which the API now reads as "leave the stored reference unchanged" rather
 * than the old full-replace "" that silently wiped it. An explicit "" still
 * clears it, and a non-empty string still replaces it — see
 * ServersPage.tsx's toServerUpdateInput for how the form's plain-string
 * field state maps onto this tri-state.
 */
export interface ServerUpdateInput {
  name: string;
  description: string;
  transport: string;
  endpointOrCommand: string;
  version: string;
  protocolVersion: string;
  secretRef?: string;
  tlsCaRef?: string;
  status: string;
}

export interface Role {
  id: string;
  name: string;
  /**
   * The optimistic-concurrency token a rename's PUT must echo in If-Match
   * (docs/plans/orbeat-role-rename-2026-08-27.md; roleDTO.RowVersion on the
   * Go side). Carried on every role response, list rows included -- there is
   * no GET /v1/admin/roles/{id} -- mirroring AdminServer.rowVersion exactly,
   * including why it is required rather than optional (a missing field is a
   * compile error under `noUncheckedIndexedAccess`, not an `undefined` that
   * would render `If-Match: "undefined"`, a guaranteed 400).
   */
  rowVersion: number;
}

/**
 * DELETE /v1/admin/roles/{id}'s 200 body (spec §6). Both counts are always
 * present and exact — deleting a role cascades to every entitlement and
 * artifact entitlement granted to it (ON DELETE CASCADE), and the portal
 * cannot compute this client-side: useEntitlements/useArtifactEntitlements
 * are capped at 100 rows by default, so a client-side count would silently
 * understate the blast radius on exactly the roles with the most grants.
 */
export interface RoleDeleteResult {
  entitlementsRevoked: number;
  artifactEntitlementsRevoked: number;
}

export interface Entitlement {
  id: string;
  roleId: string;
  mcpServerId: string;
  allowedTools: string[] | null;
  permissions: string[];
  /**
   * Optimistic-concurrency token, echoed in If-Match when editing this grant.
   * Carried on list rows (not only the by-id read), so the table a user is
   * already looking at is enough to edit from, exactly as ServersPage does.
   */
  rowVersion: number;
}

export interface AuditEvent {
  id: string;
  ts: string;
  actor: string;
  action: string;
  target: string;
  decision: string;
  metadata: Record<string, unknown>;
}

export interface AuditPage {
  events: AuditEvent[];
  limit: number;
  nextCursor: string;
}

/**
 * A cursor-paginated admin-list envelope: one page of rows keyed by the
 * endpoint's row name (e.g. "roles"), the clamped effective page size, and an
 * opaque cursor for the next page. `nextCursor` is non-empty iff the page was
 * full (`rows.length === limit`) — an exact multiple of the true row count
 * therefore yields one final EMPTY page with an empty `nextCursor`; that is
 * expected keyset-pagination behavior (see internal/api/admin_roles.go), not
 * an error. NOT the same shape as `AuditPage` above: that endpoint predates
 * this envelope, uses a different cursor encoding and different limits, and
 * is intentionally not unified with it.
 */
export type Page<K extends string, Row> = { [P in K]: Row[] } & {
  limit: number;
  nextCursor: string;
};

/**
 * GET /v1/me (internal/api/me.go): the caller's token-derived identity plus
 * edition-dependent capabilities. This interface predates this slice
 * (unused until now, the portal had no caller of /v1/me at all) and is
 * extended in place rather than duplicated: `roles` corrected to match
 * openapi.yaml's MeResponse (`nullable: true`, the same shape every other
 * roles array in this file uses), and `features` added. `features` is the
 * portal's only edition signal, see useArtifactPinningEnabled in
 * queries.ts for why the artifact minimum-revision floor controls read it
 * from here rather than from GET /v1/sync/config (which already carries the
 * same `pinning` boolean, but for orbeat-sync, a different consumer with a
 * different lifecycle).
 */
export interface Me {
  subject: string;
  email: string;
  roles: string[] | null;
  features: {
    /** Whether PUT /v1/admin/artifacts/{id}/min-revision is served here. */
    pinning: boolean;
    /**
     * Whether POST/GET/DELETE /v1/admin/virtual-keys are served here
     * (internal/api/me.go). Unlike `pinning`, which gates a handful of
     * controls inside ArtifactsPage, this gates the EXISTENCE of an entire
     * page: VirtualKeysPage renders nothing at all when this is not
     * `=== true`, see useVirtualKeysEnabled in queries.ts.
     */
    virtualKeys: boolean;
  };
}

export interface ApiError {
  error: { message: string };
}

export interface ScanFinding {
  rule: string;
  message: string;
  severity: "info" | "warn" | "block";
}

export interface AdminArtifact {
  id: string;
  type: "skill" | "subagent" | "rule";
  name: string;
  description: string;
  content: string;
  memoryScope: string | null;
  memorySeed: string | null;
  version: string;
  visibility: "org" | "role";
  approvalState: "draft" | "pending" | "approved" | "rejected";
  approved: boolean;
  approvedContent?: string;
  approvedMemoryScope?: string;
  approvedMemorySeed?: string;
  /**
   * The identity that is actually being DISTRIBUTED (migration 00016): the
   * file path on every machine receiving this artifact and the channel it
   * arrives on. These differ from the live `type`/`name`/`visibility` above
   * exactly while an identity edit waits for a second admin to approve it.
   *
   * Unlike `approvedContent`, they are carried on LIST rows too (the Go side's
   * `artifactSlimCols` keeps real values because they are slug-sized), so the
   * artifacts table can flag a pending identity change without
   * `?include=content`.
   *
   * Optional, and the absence is load-bearing: all four approved fields are
   * absent together when no snapshot exists, which the
   * `artifact_approved_identity_complete` CHECK makes an invariant. Absent
   * therefore means "nothing is distributed", never "distributed under an
   * empty name". See pendingIdentity in pages/admin/identity.ts, which never
   * compares an absent field.
   */
  approvedType?: "skill" | "subagent" | "rule";
  approvedName?: string;
  approvedVisibility?: "org" | "role";
  submittedBy?: string;
  approvedBy?: string;
  rejectReason?: string;
  scanFindings?: ScanFinding[];
  /**
   * A stable digest over `scanFindings` (docs/plans/orbeat-scan-acknowledgment-
   * 2026-08-27.md), computed and stored at submit. Absent (never an empty
   * string) exactly when `scanFindings` itself is absent -- a clean
   * submission has nothing to acknowledge, and the plan's own decision is
   * that a mandatory click on every clean artifact trains people to click
   * through, destroying the value of the click that matters.
   */
  scanFindingsDigest?: string;
  /**
   * Whether the artifact's SUBMITTER has acknowledged the CURRENT
   * `scanFindingsDigest`. Server-computed
   * (internal/api/admin_artifacts.go's toArtifactDTO:
   * `findingsAckDigest == scanFindingsDigest`), never recomputed here --
   * a client-side reimplementation of that comparison is the exact
   * staleness bug the digest exists to prevent, on a second copy of the
   * comparison with no server-side backstop. Always present (unlike
   * `scanFindingsDigest`), and `false` on an artifact with no findings at
   * all: there is nothing to acknowledge, so nothing counts as acknowledged.
   */
  findingsAcknowledged: boolean;
  /** The acknowledging actor. Present only when `findingsAcknowledged` is true. */
  findingsAckBy?: string;
  /** When the acknowledgment was recorded. Present only when `findingsAcknowledged` is true. */
  findingsAckAt?: string;
  /**
   * The optimistic-concurrency token (spec §4). Carried on every artifact
   * response — list rows included, per `artifactCols`/`artifactSlimCols` on
   * the Go side — required, not optional (see AdminServer.rowVersion for why).
   */
  rowVersion: number;
  /**
   * The admin's minimum-revision floor: the oldest approved revision any
   * developer machine may keep being served for this artifact, overriding
   * whatever it has pinned locally. 0 means NO FLOOR. Written by
   * PUT /v1/admin/artifacts/{id}/min-revision (Enterprise-only), but carried
   * on every artifact response in both editions per
   * internal/api/admin_artifacts.go's MinRevision field comment.
   *
   * Required, not optional, for the same reason AdminServer.rowVersion is:
   * 0 is the real value "no floor", so an optional field inviting
   * `minRevision ?? 0` would render "no floor" for a server that stopped
   * sending the field at all, silently hiding a floor that is actually set.
   */
  minRevision: number;
  /**
   * The per-role grants attached to this artifact. Optional because the API
   * returns it only on the single-artifact routes (GET /v1/admin/artifacts/{id}
   * and PUT of the same); list rows omit it, since a per-row count would cost
   * one query per artifact. `undefined` therefore means "this response did not
   * report it", never "no grants": a reported zero arrives as
   * `{count: 0, roles: []}`.
   */
  roleGrants?: ArtifactRoleGrants;
}

/**
 * How many roles hold a grant on an artifact, and which (capped at 50 by the
 * server; `truncated` says whether the cap bit, `count` stays exact).
 *
 * These grants are LIVE while the artifact's visibility is `role` and DORMANT
 * while it is `org`, because only role-visibility artifacts are distributed
 * through entitlements. Switching visibility never deletes them, so switching
 * an artifact back to `role` revives every dormant grant at once, with nobody
 * re-granting anything. That is what the edit form warns about before saving.
 */
export interface ArtifactRoleGrants {
  count: number;
  roles: string[];
  truncated: boolean;
}

export interface ArtifactInput {
  type: string;
  name: string;
  description: string;
  content: string;
  memoryScope: string;
  memorySeed: string;
  version: string;
  visibility: string;
}

export interface ArtifactEntitlement {
  id: string;
  roleId: string;
  artifactId: string;
}

export interface PublishStatus {
  lastAttemptAt: string | null;
  lastSuccessAt: string | null;
  lastCommit: string;
  lastError: string;
}

/**
 * The `limit` object a 402 carries
 * (docs/specs/2026-08-19-orbeat-community-caps-design.md §5). Written by two
 * places on the Go side, with identical field names: `writeLimitReached`
 * (internal/api/respond.go, the server and role caps, both write-time) and
 * `writeSeatLimitReached` (internal/authz/seatcap.go, the seat cap, which
 * fires from the resolver middleware on ANY authenticated request).
 *
 * `resource` is `"servers" | "roles" | "seats"` today and stays a plain
 * `string` here on purpose: the modal renders it verbatim, so a resource name
 * added server-side should render correctly rather than fail the client-side
 * parse and show the user nothing.
 */
export interface LimitInfo {
  resource: string;
  max: number;
  current: number;
  contact: string;
}

export interface ArtifactRevision {
  revision: number;
  source: "approval" | "rollback";
  restoredFrom?: number;
  content: string;
  memoryScope?: string;
  memorySeed?: string;
  /**
   * The identity this revision froze alongside its payload (migration 00016).
   * Named without an `approved` prefix because every field on a revision is
   * already an approved value; the Go DTO uses the same bare names.
   *
   * Rolling back restores all three, which MOVES the file on every machine
   * receiving the artifact. All three are absent together on a revision
   * approved before 00016, which recorded no identity and was deliberately not
   * backfilled: rollback then restores the content and leaves the distributed
   * identity where it is, and rollbackConfirmMessage (pages/admin/identity.ts)
   * has to say so out loud, because silence there reads as "no rename".
   */
  type?: "skill" | "subagent" | "rule";
  name?: string;
  visibility?: "org" | "role";
  approvedBy: string;
  approvedAt: string;
  isCurrent: boolean;
}

/**
 * The body of POST /v1/admin/virtual-keys (internal/api/admin_virtual_keys.ee.go,
 * docs/specs/2026-08-25-orbeat-virtual-keys-design.md sec 11). Enterprise only;
 * gated on `features.virtualKeys` (see Me above).
 *
 * NO SECRET FIELD, and there is deliberately nowhere for one to go: `jwks`
 * is the robot's PUBLIC key set, generated on the robot's own machine and
 * pasted here, never anything orbeat mints or holds.
 */
export interface VirtualKeyInput {
  name: string;
  description: string;
  roleId: string;
  /** A parsed JSON Web Key or JSON Web Key Set (RFC 7517) object, never a raw string. */
  jwks: unknown;
  /**
   * Namespaced `slug__tool` strings narrowing the key below the owning
   * role's own grant. `null` means "everything the role allows"; an empty
   * array denies every tool. Mirrors EntitlementInput.allowedTools exactly.
   */
  allowedTools: string[] | null;
}

/**
 * The admin read/write projection of a virtual key (VirtualKey schema,
 * openapi.yaml). No field here is ever a secret, in any state, see
 * VirtualKeyInput's own comment.
 */
export interface VirtualKey {
  id: string;
  /** The Keycloak client id this key's robot authenticates as. */
  clientId: string;
  roleId: string;
  name: string;
  description: string;
  /**
   * Omitted entirely (not `null`) when the key carries no narrowing;
   * mirrors virtualKeyDTO's `omitempty` on the Go side exactly, unlike
   * Entitlement.allowedTools, which is always present and nullable instead.
   */
  allowedTools?: string[];
  revoked: boolean;
  createdAt: string;
  /**
   * The optimistic-concurrency token DELETE's If-Match must quote. A
   * virtual key is never PUT; the only mutation this guards is revoke.
   */
  rowVersion: number;
}
