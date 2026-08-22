-- +goose Up
CREATE TABLE tenant (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    subject      text NOT NULL,
    email        text,
    display_name text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, subject)
);

CREATE TABLE role (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE mcp_server (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    name                text NOT NULL,
    description         text NOT NULL DEFAULT '',
    transport           text NOT NULL CHECK (transport IN ('stdio', 'http', 'sse')),
    endpoint_or_command text NOT NULL,
    version             text NOT NULL DEFAULT '',
    protocol_version    text NOT NULL DEFAULT '',
    secret_ref          text,
    status              text NOT NULL DEFAULT 'active',
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE entitlement (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    role_id       uuid NOT NULL REFERENCES role(id) ON DELETE CASCADE,
    mcp_server_id uuid NOT NULL REFERENCES mcp_server(id) ON DELETE CASCADE,
    allowed_tools text[],
    permissions   text[] NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, role_id, mcp_server_id)
);

CREATE TABLE audit_event (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    ts         timestamptz NOT NULL DEFAULT now(),
    actor      text NOT NULL,
    action     text NOT NULL,
    target     text NOT NULL DEFAULT '',
    decision   text NOT NULL CHECK (decision IN ('allow', 'deny', 'error')),
    metadata   jsonb NOT NULL DEFAULT '{}'
);

CREATE INDEX entitlement_role_idx ON entitlement (role_id);
CREATE INDEX audit_event_tenant_ts_idx ON audit_event (tenant_id, ts DESC);

-- +goose Down
DROP TABLE audit_event;
DROP TABLE entitlement;
DROP TABLE mcp_server;
DROP TABLE role;
DROP TABLE users;
DROP TABLE tenant;
