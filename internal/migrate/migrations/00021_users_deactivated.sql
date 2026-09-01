-- +goose Up
-- SCIM deprovisioning (docs/specs/2026-08-25-orbeat-scim-design.md sec 2):
-- deactivated_at is NULL for an active user, set to the moment an IdP (or a
-- future admin action) turned SCIM's `active: false` into a real access
-- decision. On its own this column is decoration -- what makes it a security
-- boundary is authz.Resolver.Resolve refusing any principal whose row has it
-- set, added in the same commit.
--
-- Deliberately NOT named "active": store.CountActiveUsers (internal/store/
-- user.go) already means something else entirely -- a user SEEN within the
-- Community seat cap's activity window, counted from last_seen_at. A boolean
-- `active` column next to that would give one word two meanings in the same
-- table, which is how a later reader writes the wrong query (e.g. filters
-- the seat count by this column instead of last_seen_at, or vice versa).
--
-- No DEFAULT and no backfill, unlike 00015's last_seen_at: NULL is the
-- correct value for every row that already exists (nobody has been
-- deactivated yet -- this is the first time orbeat can express that state at
-- all), so every existing user resolves exactly as before on upgrade. This
-- is the opposite situation from 00015, where NULL would have meant "never
-- active" and locked everyone out.
--
-- Shape matches virtual_key.revoked_at (migration 00020): a nullable
-- timestamp records not just THAT access ended but WHEN, which an operator
-- asking "when did this person lose access" needs, and which a plain
-- boolean would throw away.
ALTER TABLE users ADD COLUMN deactivated_at timestamptz;

-- +goose Down
ALTER TABLE users DROP COLUMN deactivated_at;
