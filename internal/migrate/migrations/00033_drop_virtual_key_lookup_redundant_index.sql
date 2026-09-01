-- +goose Up
-- virtual_key_lookup (00020) was created as "the gateway reads this on
-- EVERY call ... so the lookup it performs is the one that gets the index",
-- indexing (tenant_id, client_id) — but 00020 ALSO put
-- CONSTRAINT virtual_key_client_id_uniq UNIQUE (tenant_id, client_id) on the
-- same table, over the SAME two columns in the SAME order. A UNIQUE
-- constraint is backed by Postgres's own auto-generated unique B-tree index
-- (named virtual_key_client_id_uniq, same as the constraint), which serves
-- every query virtual_key_lookup could ever serve: same leading columns,
-- same order, so any plan choosing one would choose the other.
--
-- virtual_key_lookup is therefore a genuinely redundant duplicate (audit
-- B37) — the ONE item of the five in that finding with a measurable cost
-- today: every INSERT/UPDATE that touches (tenant_id, client_id) pays a
-- second B-tree insert/maintenance for an index that buys no additional
-- read ever chooses over the constraint's own index. Dropping it is a pure
-- write-cost reduction with zero read-path change (EXPLAIN before/after
-- names virtual_key_client_id_uniq either way).
DROP INDEX virtual_key_lookup;

-- +goose Down
CREATE INDEX virtual_key_lookup ON virtual_key (tenant_id, client_id);
