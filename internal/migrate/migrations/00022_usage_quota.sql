-- +goose Up
-- Usage metering and role quotas -- subsystem 4 of the gateway-parity
-- program, and the last of it (docs/specs/2026-08-25-orbeat-usage-metering-
-- design.md). Metering counts ALLOWED tool calls only (section 1): a call
-- denied by RBAC, a revoked virtual key, or the interceptor never reached
-- the upstream and did no work, so it is never written here.
--
-- CORRECTED before this migration ever ran outside an ephemeral test
-- container: Task 1's first draft made usage_daily per-SUBJECT only, because
-- orbeat resolves role membership per-request from the token
-- (internal/authz/resolver.go) rather than persisting a durable
-- subject-to-role mapping to join against -- at the time, nothing tied a
-- call to a role at all. That made the per-role monthly quota (section 2)
-- unbuildable as designed: MonthlyCallsForRole had no role_id column to sum.
-- The fix needs no persisted subject-to-role mapping -- it attributes each
-- call to the role that AUTHORIZED it. store.Entitlement already carries
-- RoleID (internal/store/rbac.go): the entitlement that matched during the
-- per-call RBAC decision (rbac.AuthorizingEntitlement) names that call's
-- role exactly. One call, one authorizing role, no double counting for a
-- subject holding several.
--
-- usage_daily is the durable target an in-process counter
-- (internal/gateway/usage.ee.go) flushes to periodically and on shutdown --
-- never a synchronous write on the tool-call hot path (section 3). One row
-- per (tenant, day, subject, server, tool, role); no surrogate key, because
-- that six-column tuple IS the bucket's identity and every write against it
-- is an upsert (section 1, section 4): "the natural key is the identity of
-- a bucket." subject is the token's sub claim, for a robot exactly as for a
-- human, and stays a plain, non-FK column: internal/gateway/server.go builds
-- every session with `subject: p.Subject` and nothing else ever writes that
-- field, so the value rbac_middleware.go hands UsageCounter.Count is that
-- claim, whoever is calling. It is NOT a virtual key's client_id, which this
-- header claimed until 2026-08-30 (A10): client_id is p.ClientID, which
-- internal/auth/principal.go reads from a different claim, azp, so filtering
-- this column by a client_id returns nothing.
--
-- role_id is what turns a per-subject count into an attributable, per-role
-- one: the SAME subject calling the SAME tool via TWO different roles (a
-- human holding two realm roles that both entitle the same server) produces
-- TWO rows, not one, so each role's monthly total sums only the calls IT
-- authorized -- never a subject's total regardless of which role granted it.
--
-- server_id and role_id both carry COMPOSITE FKs, (tenant_id, server_id) ->
-- mcp_server(tenant_id, id) and (tenant_id, role_id) -> role(tenant_id, id),
-- for the same reason 00010 added the same shape to
-- entitlement/artifact_entitlement and 00020 added it to virtual_key: a
-- plain `server_id -> mcp_server(id)` (or `role_id -> role(id)`) only proves
-- the referenced row exists SOMEWHERE, not that it belongs to this row's own
-- tenant.
CREATE TABLE usage_daily (
    tenant_id uuid   NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    day       date   NOT NULL,
    subject   text   NOT NULL,
    server_id uuid   NOT NULL,
    tool      text   NOT NULL,
    role_id   uuid   NOT NULL,
    calls     bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, day, subject, server_id, tool, role_id),
    CONSTRAINT usage_daily_server_tenant_fk
        FOREIGN KEY (tenant_id, server_id) REFERENCES mcp_server (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT usage_daily_role_tenant_fk
        FOREIGN KEY (tenant_id, role_id) REFERENCES role (tenant_id, id) ON DELETE CASCADE
);

-- Serves MonthlyCallsForRole's `WHERE tenant_id=$1 AND role_id=$2 AND day >=
-- $3 AND day < $4` -- a role's calendar-month total, the query quota
-- enforcement (a later task) reads -- and doubles as the index
-- usage_daily_role_tenant_fk's CASCADE lookup needs on role deletion. Same
-- dual-purpose reasoning 00012 applied adding
-- entitlement_tenant_role_id_idx / artifact_entitlement_tenant_role_id_idx:
-- without it, a role's monthly total scans every one of the tenant's
-- usage_daily rows (bounded by tenant_id via the primary key, but not by
-- role_id), not just the ones belonging to that role.
CREATE INDEX usage_daily_tenant_role_day_idx ON usage_daily (tenant_id, role_id, day);

-- role_quota: an optional monthly call cap on a role (section 2 -- "per
-- role, not per user, because that is the unit orbeat already grants access
-- with"). id is a synthetic PK, mirroring virtual_key (00020), so the
-- bump_row_version trigger below (declared 00013_row_version.sql:13, applied
-- 00013_row_version.sql:27-28) has a stable row to bump for optimistic
-- concurrency on the admin PUT/DELETE routes a later task adds.
--
-- UNIQUE(tenant_id, role_id) is both the business key (at most one quota row
-- per role) and the index the composite FK's CASCADE lookup needs on role
-- deletion. Unlike virtual_key (00020), which needed a SEPARATE
-- (tenant_id, role_id) index because ITS OWN uniqueness is on client_id, not
-- role_id, this table's natural key already covers both jobs with the one
-- index Postgres creates to back the UNIQUE constraint -- no second index is
-- needed here.
--
-- The FK is COMPOSITE, (tenant_id, role_id) -> role(tenant_id, id), for the
-- exact reason 00020's virtual_key_role_tenant_fk is: a plain
-- `role_id -> role(id)` reopens the cross-tenant gap 00010 closed for
-- entitlement/artifact_entitlement -- it would only prove the role exists
-- somewhere, not that it belongs to the tenant naming it in this row.
-- role_tenant_id_uniq (00010) is the parent side both this and 00020
-- reference.
CREATE TABLE role_quota (
    id            uuid   PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid   NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    role_id       uuid   NOT NULL,
    monthly_calls bigint NOT NULL,
    row_version   bigint NOT NULL DEFAULT 1,
    CONSTRAINT role_quota_tenant_role_uniq UNIQUE (tenant_id, role_id),
    CONSTRAINT role_quota_role_tenant_fk
        FOREIGN KEY (tenant_id, role_id) REFERENCES role (tenant_id, id) ON DELETE CASCADE
);

CREATE TRIGGER role_quota_bump_row_version BEFORE UPDATE ON role_quota
    FOR EACH ROW EXECUTE FUNCTION bump_row_version();

-- +goose Down
DROP TABLE role_quota;
DROP TABLE usage_daily;
