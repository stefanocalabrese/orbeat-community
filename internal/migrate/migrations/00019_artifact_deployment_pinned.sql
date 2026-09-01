-- +goose Up
-- Spec sec 9.4 (docs/specs/2026-08-22-orbeat-artifact-version-pinning-
-- design.md): one boolean recording WHY an install is on the revision it
-- reported, not merely which one. An install reporting revision 4 while
-- latest is 6 is either a developer deliberately holding back or a laptop
-- nobody has opened since revision 5 landed, and those need opposite
-- responses. Without this column the registry cannot tell them apart at all.
--
-- pinned means "this install applied this revision because a local pin
-- (~/.config/orbeat/pins.json) named it", set by the client on
-- POST /v1/sync/deployments and nothing else: it is self-asserted, the same
-- trust level as every other value on this row (00017's own comment), so a
-- forged true changes a number on an admin page and nothing else.
--
-- NOT NULL DEFAULT false, matching every existing column on this table:
-- there is no meaningful null here, and a report always states one or the
-- other.
--
-- A BOOLEAN, NOT THE REQUESTED REVISION. Carrying the developer's requested
-- revision here as well would let an admin see which version a named
-- developer wanted, which answers no operator question this feature has and
-- is a step toward the per-person drill-down sec 8.4 (registry design)
-- recommends against. The requested revision already lives on the
-- developer's own machine, in pins.json, which is the only place it needs to.
ALTER TABLE artifact_deployment ADD COLUMN pinned boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE artifact_deployment DROP COLUMN pinned;
