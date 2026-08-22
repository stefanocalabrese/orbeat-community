import { ApiRequestError } from "../../api/client";

/**
 * True iff `e` is a 412 from the optimistic-concurrency precondition (spec
 * §5) — the write's `If-Match` no longer matches the row's current
 * `row_version`.
 *
 * Deliberately NOT treated as "someone else edited this" anywhere it is
 * used: TestNoOpUpdateStillBumpsRowVersion (internal/store/concurrency_test.go)
 * pins that the trigger bumps `row_version` on EVERY update, including one
 * that writes back the exact values already there. A client retry after a
 * dropped response (e.g. a network timeout) therefore produces a 412 with
 * nobody else involved — the first attempt already succeeded, the client
 * just never heard about it. ConflictNotice's copy (see ConflictNotice.tsx)
 * is written to stay true in that case too.
 *
 * Kept in its own module, separate from the ConflictNotice component: a
 * .tsx file that exports both a component and a plain function trips
 * react-refresh/only-export-components (fast refresh needs a component-only
 * module to know what to re-render).
 */
export function isConflict(e: unknown): boolean {
  return e instanceof ApiRequestError && e.status === 412;
}
