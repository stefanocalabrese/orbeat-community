-- +goose Up
-- Community edition seat cap (docs/specs/2026-08-19-orbeat-community-caps-
-- design.md sec 3.2): a seat is a user who authenticated within the last 7
-- days, so the count self-heals as users go idle. DEFAULT now() backfills
-- every EXISTING row to the moment this migration runs (one now() evaluated
-- once for the ALTER TABLE, not a per-row default) rather than leaving them
-- null -- a null would read as "never active" and lock every existing user
-- out of an upgraded Community install on day one.
ALTER TABLE users ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now();

-- Serves the seat-count query store.CountActiveUsers issues
-- (WHERE tenant_id = $1 AND last_seen_at > $2). users' only existing index is
-- the UNIQUE(tenant_id, subject) constraint from 00001: a usable prefix for
-- tenant_id alone, but it cannot serve the last_seen_at range filter without
-- scanning every row for that tenant. Community caps a tenant at 10 seats,
-- where that scan costs nothing either way, but Enterprise carries no such
-- cap (spec sec 4) and can have far more users -- and the index is cheap to
-- maintain either way: one bounded-width btree entry, touched no more than
-- once per user per hour by UpsertUser's staleness gate (store/user.go), not
-- on every authenticated request.
CREATE INDEX users_tenant_last_seen_idx ON users (tenant_id, last_seen_at);

-- +goose Down
DROP INDEX users_tenant_last_seen_idx;
ALTER TABLE users DROP COLUMN last_seen_at;
