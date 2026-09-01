-- +goose Up
-- Per-rule project targeting: a rule can name the tags of the projects it
-- applies to, and orbeat-sync writes it only into projects carrying at least
-- one of them. NULL means "every registered project", which is what every rule
-- shipped before this does, so the column's absence is the old behaviour.
--
-- The TAGS THEMSELVES ARE LOCAL, declared by the developer on their own machine
-- (`orbeat-sync project add ~/work/api --tag go`), and orbeat never learns them.
-- That asymmetry is the design, not a limitation: the admin says WHAT KIND of
-- project a rule is for, the developer says what kind their projects are, and
-- nothing about one developer's filesystem has to be modelled server-side for
-- the two to meet. A path-glob design would have put `/Users/alice/work/*` in
-- the catalog, where it is wrong for every other machine.
ALTER TABLE artifact
    ADD COLUMN target_tags text[]
        CHECK (target_tags IS NULL OR
               (array_length(target_tags, 1) BETWEEN 1 AND 16 AND type = 'rule')),
    ADD COLUMN approved_target_tags text[]
        CHECK (approved_target_tags IS NULL OR
               (array_length(approved_target_tags, 1) BETWEEN 1 AND 16 AND approved_type = 'rule'));

-- Snapshotted like every other distribution-affecting field (migration 00016):
-- re-targeting an approved rule is a change to WHO RECEIVES IT, so it waits for
-- an approval exactly as a visibility flip does. Without this column an admin
-- could widen a reviewed rule to every project in the org with no second pair
-- of eyes, which is the hole 00016 closed for name/type/visibility.
--
-- Element shape (a lowercase slug) is validated in the API rather than here: a
-- CHECK cannot contain a subquery, so `unnest`-ing the array to regex each
-- element is not expressible. Arity and the rule-only restriction are, and they
-- are what stop the column being abused as a general-purpose bag.

-- The revision chain carries it for the same reason it carries visibility
-- (00016): a rollback restores what was approved, and a rollback that put back
-- old content under today's targeting would restore half of a decision.
ALTER TABLE artifact_revision
    ADD COLUMN target_tags text[];

-- +goose Down
ALTER TABLE artifact_revision DROP COLUMN target_tags;
ALTER TABLE artifact DROP COLUMN target_tags, DROP COLUMN approved_target_tags;
