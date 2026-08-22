-- +goose Up
CREATE TABLE artifact (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    type         text NOT NULL CHECK (type IN ('skill', 'subagent')),
    name         text NOT NULL,
    description  text NOT NULL DEFAULT '',
    content      text NOT NULL,
    memory_scope text CHECK (memory_scope IS NULL OR memory_scope IN ('user', 'project', 'local')),
    version      text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, type, name),
    CHECK (memory_scope IS NULL OR type = 'subagent')
);

CREATE INDEX artifact_tenant_active_idx ON artifact (tenant_id) WHERE status = 'active';

CREATE TABLE publish_state (
    tenant_id       uuid PRIMARY KEY REFERENCES tenant(id) ON DELETE CASCADE,
    last_attempt_at timestamptz,
    last_success_at timestamptz,
    last_commit     text NOT NULL DEFAULT '',
    last_error      text NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE publish_state;
DROP TABLE artifact;
