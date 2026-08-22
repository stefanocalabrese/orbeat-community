-- +goose Up
-- Indexes matching the keyset sort orders introduced by admin-list pagination.
-- Every paginated list orders by `<sort keys…>, id`, so each index ends in id:
-- without it Postgres can seek to the key prefix but must still sort the ties.

-- artifact: ORDER BY type, name, id. artifact already has artifact_pkey,
-- artifact_tenant_id_uniq, and UNIQUE(tenant_id,type,name) — that last one IS
-- a usable path for this query, but only at shallow cursors: verified against
-- a real Postgres, the planner abandons it at depth and falls back to a
-- bitmap scan + top-N sort (22.7 ms vs 0.041 ms for the purpose-built index).
-- This index isn't closing a gap so much as buying plan stability across the
-- whole cursor range, not just its first few pages.
CREATE INDEX artifact_tenant_type_name_id_idx
    ON artifact (tenant_id, type, name, id);

-- artifact, filtered: the `state` filter moves from a Go loop into SQL, so the
-- filtered page needs its own key. approval_state leads because it is an
-- equality predicate; the sort keys follow.
CREATE INDEX artifact_tenant_state_type_name_id_idx
    ON artifact (tenant_id, approval_state, type, name, id);

-- entitlement: 00001 created `entitlement_role_idx (role_id)` — no tenant_id,
-- so it cannot serve the tenant-scoped page at all.
--
-- That index is deliberately KEPT, not replaced: role_id is an ON DELETE
-- CASCADE foreign key, and a cascade deletes by bare `role_id = X`. The new
-- index leads with tenant_id and therefore does NOT serve that lookup. Dropping
-- entitlement_role_idx would turn every role deletion into a seq scan.
CREATE INDEX entitlement_tenant_role_id_idx
    ON entitlement (tenant_id, role_id, id);

-- artifact_entitlement: 00004's (tenant_id, role_id) is a strict PREFIX of the
-- new (tenant_id, role_id, id) key, so any query the old index could serve
-- the new one serves too — replacing it is safe by prefix subsumption alone.
-- This one DOES still serve a live consumer — 00010's composite FK
-- artifact_entitlement_role_tenant_fk FOREIGN KEY (tenant_id, role_id), whose
-- referential-integrity check is exactly `tenant_id = $1 AND role_id = $2` —
-- but the replacement covers it just as well, unlike entitlement_role_idx
-- above, which cannot be replaced because its consumer's lookup (a bare
-- `role_id = X` cascade delete) doesn't share a leading column with any
-- tenant-scoped index.
DROP INDEX artifact_entitlement_role_idx;
CREATE INDEX artifact_entitlement_tenant_role_id_idx
    ON artifact_entitlement (tenant_id, role_id, id);

-- DEFERRED, OUT OF SCOPE, NOTED HERE ON PURPOSE: artifact_entitlement.role_id
-- (00004) is a bare `REFERENCES role(id) ON DELETE CASCADE` — the exact same
-- shape as entitlement.role_id above — but unlike entitlement it has NO
-- bare-role_id index backing that cascade, only the tenant-leading ones this
-- migration creates/keeps. The exact seq scan that entitlement_role_idx
-- exists to prevent already happens here, today, on every role deletion
-- (verified: `Seq Scan on artifact_entitlement, Rows Removed by Filter:
-- 79996`). This migration is where cascade indexing is reasoned about, so
-- this gap is recorded rather than passed over — see
-- docs/future-features.md. Left unfixed because it needs its own decision
-- (add a bare role_id index vs. accept the scan at current table sizes), not
-- a silent add riding along with this migration.

-- artifact_revision needs none: UNIQUE(artifact_id, revision_num) already
-- covers `WHERE artifact_id=$1 ORDER BY revision_num DESC`.
--
-- role and mcp_server need none: both have UNIQUE(tenant_id, name), which
-- seeks the page start. Note that "names are unique per tenant" is NOT what
-- distinguishes them from artifact above — UNIQUE(tenant_id, type, name) is
-- equally unique per tenant, and still needed its own index. The real
-- distinction, also verified against a real Postgres, is arity: mcp_server's
-- single extra sort column (name) past tenant_id gives the planner a clean
-- scalar index bound to seek on, where artifact's two extra columns (type,
-- name) make it a row-comparison bound the planner's estimator handles far
-- worse at depth — the same shallow-vs-deep collapse described above.

-- +goose Down
DROP INDEX artifact_entitlement_tenant_role_id_idx;
CREATE INDEX artifact_entitlement_role_idx ON artifact_entitlement (tenant_id, role_id);
DROP INDEX entitlement_tenant_role_id_idx;
DROP INDEX artifact_tenant_state_type_name_id_idx;
DROP INDEX artifact_tenant_type_name_id_idx;
