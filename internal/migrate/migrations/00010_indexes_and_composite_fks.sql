-- +goose Up
-- 1. Restore the distribution hot-path index that migration 00006 silently
-- destroyed. 00003 created a partial index `artifact_tenant_active_idx ...
-- WHERE status = 'active'`; 00006's `DROP COLUMN status` took that index with
-- it, leaving the Channel-1/Channel-2 read paths (which filter on
-- `approved_content IS NOT NULL`) with no purpose-built index. This partial
-- index covers exactly the distributable set, keyed by the columns those reads
-- filter and order by.
CREATE INDEX artifact_tenant_distributable_idx
    ON artifact (tenant_id, visibility)
    WHERE approved_content IS NOT NULL;

-- 2. Give the audit keyset index its tiebreak column. store/audit.go paginates
-- `ORDER BY ts DESC, id DESC`, but 00001's index was only `(tenant_id, ts DESC)`
-- — so the id tiebreak fell out of the index and every page did an extra sort.
-- Replace it with the full keyset key.
DROP INDEX audit_event_tenant_ts_idx;
CREATE INDEX audit_event_tenant_ts_id_idx
    ON audit_event (tenant_id, ts DESC, id DESC);

-- 3. Composite-FK backstops for cross-tenant consistency. Today only the Go
-- handlers enforce that an entitlement's role/server (and an artifact
-- entitlement's role/artifact) belong to the SAME tenant as the entitlement;
-- the DB does not. These composite FKs make it the schema's last line.
--
-- A composite FK must reference an exactly-matching UNIQUE (or PK) constraint,
-- and none of these parents has a unique key on (tenant_id, id) — only the PK
-- on (id) alone. So we first add UNIQUE (tenant_id, id) to each parent. That
-- constraint is REDUNDANT for uniqueness (id alone is already the PK, hence
-- unique), but it is NOT a removable duplicate: it exists solely to be the
-- referenced target of the composite FKs below, which Postgres cannot create
-- without it.
ALTER TABLE role       ADD CONSTRAINT role_tenant_id_uniq       UNIQUE (tenant_id, id);
ALTER TABLE mcp_server ADD CONSTRAINT mcp_server_tenant_id_uniq UNIQUE (tenant_id, id);
ALTER TABLE artifact   ADD CONSTRAINT artifact_tenant_id_uniq   UNIQUE (tenant_id, id);

-- The composite FKs are ADDED alongside — not in place of — the existing
-- single-column FKs (role_id -> role(id), mcp_server_id -> mcp_server(id),
-- etc.). They are not duplicates: the existing FKs constrain the child column to
-- SOME parent row; these additionally pin it to a parent row in the child's OWN
-- tenant. ON DELETE CASCADE matches the existing single-column FKs so no delete
-- that succeeds today starts failing (deleting a role/server/artifact still
-- cascades its entitlements, now via either FK).
ALTER TABLE entitlement
    ADD CONSTRAINT entitlement_role_tenant_fk
        FOREIGN KEY (tenant_id, role_id)       REFERENCES role       (tenant_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT entitlement_mcp_server_tenant_fk
        FOREIGN KEY (tenant_id, mcp_server_id) REFERENCES mcp_server (tenant_id, id) ON DELETE CASCADE;

ALTER TABLE artifact_entitlement
    ADD CONSTRAINT artifact_entitlement_role_tenant_fk
        FOREIGN KEY (tenant_id, role_id)     REFERENCES role     (tenant_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT artifact_entitlement_artifact_tenant_fk
        FOREIGN KEY (tenant_id, artifact_id) REFERENCES artifact (tenant_id, id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE artifact_entitlement
    DROP CONSTRAINT artifact_entitlement_artifact_tenant_fk,
    DROP CONSTRAINT artifact_entitlement_role_tenant_fk;
ALTER TABLE entitlement
    DROP CONSTRAINT entitlement_mcp_server_tenant_fk,
    DROP CONSTRAINT entitlement_role_tenant_fk;
ALTER TABLE artifact   DROP CONSTRAINT artifact_tenant_id_uniq;
ALTER TABLE mcp_server DROP CONSTRAINT mcp_server_tenant_id_uniq;
ALTER TABLE role       DROP CONSTRAINT role_tenant_id_uniq;

DROP INDEX audit_event_tenant_ts_id_idx;
CREATE INDEX audit_event_tenant_ts_idx ON audit_event (tenant_id, ts DESC);

DROP INDEX artifact_tenant_distributable_idx;
