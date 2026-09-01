-- +goose Up
-- A virtual key is a credential for a ROBOT: a CI job, a script, an unattended
-- agent. The credential itself is a Keycloak client authenticating by signed
-- assertion (private_key_jwt); this table holds only the POLICY, keyed on that
-- client_id. See docs/specs/2026-08-25-orbeat-virtual-keys-design.md.
--
-- NO SECRET COLUMN, AND THERE NEVER WILL BE ONE. The robot holds a private key
-- orbeat never sees. registration_access_token_ref is a secretRef (the same
-- scheme-routed form mcp_server.secret_ref uses), never a token value.
--
-- role_id is the CAP: a key can never exceed the role's live entitlements. The
-- FK is COMPOSITE, (tenant_id, role_id) -> role(tenant_id, id), not a plain
-- role_id -> role(id): 00010 added that same composite shape to
-- entitlement/artifact_entitlement as a schema-level backstop against a
-- cross-tenant role_id (until then only the Go handlers enforced tenancy), and
-- a table starting fresh here should not reintroduce the gap that migration
-- was written to close. role_tenant_id_uniq (00010) is the parent side this
-- references; no separate plain role_id FK is added alongside it, because the
-- composite one already constrains role_id to an existing role AND pins it to
-- this row's own tenant in one constraint -- a single-column FK would let a
-- handler that forgets the tenant check hand a robot another tenant's role.
-- ON DELETE CASCADE because a key outliving its role would be a grant nobody
-- can see or audit.
CREATE TABLE virtual_key (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    client_id   text NOT NULL,
    role_id     uuid NOT NULL,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    -- NAMESPACED, e.g. "github__create_issue": one key narrows tools across
    -- every server its role can see, and a bare tool name is not unique
    -- across servers -- a review probe on Task 1's predicate found that a flat,
    -- unfiltered list of bare names is server-blind (a key narrowed to "read"
    -- would allow "read" on every server the role grants), so the gateway
    -- MUST slug-filter this column to bare per-server names before any of it
    -- reaches rbac.KeyToolAllowed (Task 4). NOT entitlement.allowed_tools'
    -- shape (that column sits ON one server-scoped row and is already bare)
    -- -- only the nil/non-nil/empty MEANING matches: NULL means every tool the
    -- role allows; non-null narrows; empty denies all. See spec section 6,
    -- "the second trap".
    allowed_tools text[],
    registration_access_token_ref text NOT NULL DEFAULT '',
    revoked_at  timestamptz,
    created_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    row_version bigint NOT NULL DEFAULT 1,
    CONSTRAINT virtual_key_client_id_uniq UNIQUE (tenant_id, client_id),
    CONSTRAINT virtual_key_role_tenant_fk
        FOREIGN KEY (tenant_id, role_id) REFERENCES role (tenant_id, id) ON DELETE CASCADE
);

-- The gateway reads this on EVERY call (revocation must not wait for a session
-- to expire), so the lookup it performs is the one that gets the index.
CREATE INDEX virtual_key_lookup ON virtual_key (tenant_id, client_id);

-- Serves virtual_key_role_tenant_fk's CASCADE lookup on role deletion, the
-- same reason 00012 gave entitlement/artifact_entitlement a (tenant_id,
-- role_id) index: without it, deleting a role would seq-scan this table to
-- find the rows to cascade.
CREATE INDEX virtual_key_tenant_role_id_idx ON virtual_key (tenant_id, role_id);

CREATE TRIGGER virtual_key_bump_row_version BEFORE UPDATE ON virtual_key
    FOR EACH ROW EXECUTE FUNCTION bump_row_version();

-- +goose Down
DROP TABLE virtual_key;
