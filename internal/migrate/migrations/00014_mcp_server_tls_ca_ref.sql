-- +goose Up
-- Per-upstream TLS trust (fable-audit §7 #14). Holds a SECRET REFERENCE to a CA
-- certificate in PEM form, never certificate bytes — the DB stores references
-- only. NULL means the system CA pool, which is the pre-existing behaviour and
-- stays byte-for-byte unchanged.
ALTER TABLE mcp_server ADD COLUMN tls_ca_ref text;

-- +goose Down
ALTER TABLE mcp_server DROP COLUMN tls_ca_ref;
