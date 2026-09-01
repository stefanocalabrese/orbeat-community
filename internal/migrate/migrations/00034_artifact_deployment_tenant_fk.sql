-- +goose Up
-- artifact_deployment (00017) was the other child of artifact 00010's sweep
-- missed alongside artifact_revision (00032 closed that one; this is the
-- second and, per cascade_index_test.go's inboundForeignKeys["artifact"],
-- the last): it carries a bare artifact_id -> artifact(id) FK with no
-- composite (tenant_id, artifact_id) -> artifact(tenant_id, id) backstop, so
-- today only the Go handlers enforce that a deployment row's artifact_id
-- belongs to the same tenant as the deployment row itself, and the DB does
-- not.
--
-- artifact already carries the UNIQUE (tenant_id, id) target this FK needs
-- (artifact_tenant_id_uniq, added by 00010) — no ALTER TABLE ADD CONSTRAINT
-- UNIQUE is needed on the parent side here.
--
-- ADDED alongside, not in place of, the existing single-column FK
-- (artifact_id -> artifact(id)): that FK still constrains artifact_id to
-- SOME artifact row; this one additionally pins it to the artifact's own
-- tenant. ON DELETE CASCADE matches the existing FK and 00010/00032's own
-- convention, so no delete that succeeds today starts failing.
ALTER TABLE artifact_deployment
    ADD CONSTRAINT artifact_deployment_artifact_tenant_fk
        FOREIGN KEY (tenant_id, artifact_id) REFERENCES artifact (tenant_id, id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE artifact_deployment
    DROP CONSTRAINT artifact_deployment_artifact_tenant_fk;
