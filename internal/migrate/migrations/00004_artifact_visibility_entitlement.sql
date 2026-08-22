-- +goose Up
ALTER TABLE artifact
    ADD COLUMN visibility text NOT NULL DEFAULT 'org'
        CHECK (visibility IN ('org', 'role'));

CREATE TABLE artifact_entitlement (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    role_id     uuid NOT NULL REFERENCES role(id) ON DELETE CASCADE,
    artifact_id uuid NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, role_id, artifact_id)
);

CREATE INDEX artifact_entitlement_role_idx ON artifact_entitlement (tenant_id, role_id);

-- +goose Down
DROP TABLE artifact_entitlement;
ALTER TABLE artifact DROP COLUMN visibility;
