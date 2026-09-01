-- +goose Up
-- artifact_revision (00007) is the only child of artifact WITHOUT the
-- composite (tenant_id, X) -> parent(tenant_id, id) foreign key 00010 added
-- to every other artifact/role/mcp_server child (entitlement,
-- artifact_entitlement) as a schema-level backstop against a cross-tenant
-- child row: today only the Go handlers enforce that a revision's
-- artifact_id belongs to the same tenant as the revision row itself, and the
-- DB does not (audit B37).
--
-- artifact already carries the UNIQUE (tenant_id, id) target this FK needs
-- (artifact_tenant_id_uniq, added by 00010) — no second ALTER TABLE ADD
-- CONSTRAINT UNIQUE is needed on the parent side here, unlike 00010 itself,
-- which had to add that constraint to three tables in the same migration
-- that first referenced it.
--
-- ADDED alongside, not in place of, the existing single-column FK
-- (artifact_id -> artifact(id)): that FK still constrains artifact_id to
-- SOME artifact row; this one additionally pins it to the artifact's own
-- tenant. ON DELETE CASCADE matches the existing FK and 00010's own
-- convention, so no delete that succeeds today starts failing.
ALTER TABLE artifact_revision
    ADD CONSTRAINT artifact_revision_artifact_tenant_fk
        FOREIGN KEY (tenant_id, artifact_id) REFERENCES artifact (tenant_id, id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE artifact_revision
    DROP CONSTRAINT artifact_revision_artifact_tenant_fk;
