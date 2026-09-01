-- +goose Up
-- audit B9's shipped fix (internal/store/user.go's UpsertProvisionedUser)
-- needed a value for "provisioned, never authenticated" and 00015 had already
-- made last_seen_at NOT NULL DEFAULT now(), with a migration out of that
-- lane's scope. It used a documented stand-in instead: the Unix epoch
-- (time.Unix(0,0).UTC(), store.neverAuthenticated), far enough in the past to
-- fall outside any activeSeatWindow (authz/seatcap.go) this product will ever
-- configure. That comment named NULL as "the schema-correct shape" and
-- deferred it. This migration is that deferred change.
--
-- 1. Make the column nullable. DEFAULT now() is left in place, deliberately:
-- it is still correct for UpsertUser's INSERT branch (a fresh row there IS a
-- genuine first authentication, the same event the default already encodes),
-- and dropping it would force that statement to start naming the column
-- explicitly for no behavioural gain.
ALTER TABLE users ALTER COLUMN last_seen_at DROP NOT NULL;

-- 2. Convert the sentinel written under the old scheme into its schema-correct
-- replacement. Exact-value equality only, on purpose: '1970-01-01 00:00:00+00'
-- is a single literal that nothing in this codebase writes to last_seen_at
-- except UpsertProvisionedUser's INSERT branch (its UPDATE branch never
-- touches the column at all, by the same design that keeps a SCIM PATCH from
-- resetting a real activity clock). Every genuine authentication goes through
-- UpsertUser, whose write path sets last_seen_at = now() unconditionally
-- whenever it writes -- so no real login timestamp can equal this literal by
-- coincidence, and this UPDATE touches exactly the set of rows that mean
-- "provisioned, never authenticated" and nothing else. A row this predicate
-- does not match is a real timestamp and is left untouched.
UPDATE users SET last_seen_at = NULL
    WHERE last_seen_at = '1970-01-01 00:00:00+00'::timestamptz;

-- +goose Down
-- Restore the sentinel for every row this Up nulled, so NOT NULL can be
-- reapplied without failing on a null row, and so a downgraded deployment
-- keeps behaving exactly as it did before this migration (CountActiveUsers'
-- `last_seen_at > since` predicate excludes the epoch sentinel exactly as it
-- excludes NULL, so nothing downstream depends on which of the two the row
-- carries).
UPDATE users SET last_seen_at = '1970-01-01 00:00:00+00'::timestamptz
    WHERE last_seen_at IS NULL;
ALTER TABLE users ALTER COLUMN last_seen_at SET NOT NULL;
