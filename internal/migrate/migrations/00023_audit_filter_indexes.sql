-- +goose Up
-- `GET /v1/admin/audit` gained ?actor= and ?action= filters, which become real
-- equality predicates in auditPageSQL rather than the `$n IS NULL OR col = $n`
-- shape the export's date bounds use, precisely so an index can serve them.
--
-- Both are measured, not assumed. On 100,000 events in one tenant (20 actors,
-- 30 actions, decisions 90/9/1) with LIMIT 100, postgres:18-alpine:
--
--   filter                      before          after
--   actor, 5% of rows           0.155 ms        0.054 ms   (Index Scan Backward
--                                                           on audit_event_ts_idx
--                                                           -> this index)
--   action, 3% of rows          0.179 ms        0.050 ms
--   actor with 5 rows total     2.757 ms        0.019 ms   SEQ SCAN -> index
--   action with 5 rows total    2.644 ms        0.016 ms   SEQ SCAN -> index
--
-- The selective case is the one that matters, and it is also the question the
-- filter exists to answer: "what did this one person do", where that person
-- appears a handful of times in a table that only ever grows. Every seq scan
-- measured came from a selective predicate; the 3-9% cases never seq-scanned,
-- because walking audit_event_ts_idx in ts order and filtering already finds
-- 100 matches quickly.
CREATE INDEX audit_event_tenant_actor_ts_id_idx
    ON audit_event (tenant_id, actor, ts DESC, id DESC);

CREATE INDEX audit_event_tenant_action_ts_id_idx
    ON audit_event (tenant_id, action, ts DESC, id DESC);

-- DELIBERATELY NOT INDEXED: decision. It is CHECK-constrained to three values
-- ('allow', 'deny', 'error'), so a decision-only filter can never be selective
-- enough to seq-scan: the worst measured case, decision = 'error' at 1% of
-- rows, walks audit_event_ts_idx in 0.536 ms, and a third index moved that to
-- 0.073 ms, buying a fraction of a millisecond on the least valuable query in
-- exchange for a third B-tree insert on every audited request. A decision
-- filter combined with actor or action rides the index above instead
-- (measured: 0.010 ms). Revisit only if `decision` ever gains a high-cardinality
-- value, which would mean the CHECK changed.

-- +goose Down
DROP INDEX audit_event_tenant_action_ts_id_idx;
DROP INDEX audit_event_tenant_actor_ts_id_idx;
