-- +goose Up
-- ListVirtualKeysPage/virtualKeyPageSQL orders by (name, id) and has done so
-- since migration 00020 introduced the table -- but 00020 gave virtual_key
-- only virtual_key_lookup (tenant_id, client_id) and
-- virtual_key_tenant_role_id_idx (tenant_id, role_id), neither of which is a
-- prefix of (tenant_id, name, id). Unlike role/mcp_server, name is NOT part
-- of any UNIQUE constraint here (only (tenant_id, client_id) is unique), so
-- there was never an auto-generated index riding along for free either: the
-- production default sort has been running unindexed (Seq Scan + Sort at
-- scale) since the table was created, invisible because a small deployment's
-- virtual-key count never forced the planner to notice.
--
-- docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 2 requires every
-- allowlisted ?sort value to be servable by an existing index, and this
-- list's only allowlisted value is its existing default ("name") -- so this
-- index is not a new capability riding along with that feature, it is the
-- precondition for offering the feature at all without repeating v1.22.0's
-- headline defect (an unindexed sort silently degrading to Seq Scan + Sort).
CREATE INDEX virtual_key_tenant_name_id_idx ON virtual_key (tenant_id, name, id);

-- +goose Down
DROP INDEX virtual_key_tenant_name_id_idx;
