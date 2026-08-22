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

export interface Role {
  id: string;
  name: string;
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

export interface Me {
  subject: string;
  email: string;
  roles: string[];
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
  submittedBy?: string;
  approvedBy?: string;
  rejectReason?: string;
  scanFindings?: ScanFinding[];
  /**
   * The optimistic-concurrency token (spec §4). Carried on every artifact
   * response — list rows included, per `artifactCols`/`artifactSlimCols` on
   * the Go side — required, not optional (see AdminServer.rowVersion for why).
   */
  rowVersion: number;
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
  approvedBy: string;
  approvedAt: string;
  isCurrent: boolean;
}
