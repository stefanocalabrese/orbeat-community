import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import { config } from "../config";
import type { LimitInfo } from "./types";

export class ApiRequestError extends Error {
  status: number;

  /**
   * The structured Community-edition cap payload parsed out of a 402 body
   * (docs/specs/2026-08-19-orbeat-community-caps-design.md §5), and undefined
   * for every other status.
   *
   * apiFetch below sets it ONLY on a 402, which is what lets `limitInfo` and
   * the cache wiring ask "did the server report a cap?" without a second copy
   * of the status literal: the sole comparison against the cap status anywhere
   * in the portal's source is apiFetch's `res.status === 402`, so nothing can
   * drift away from it. (The number itself appears in plenty of test fixtures
   * and prose, this file's comments included; what is singular is the
   * comparison.)
   */
  limit?: LimitInfo;

  constructor(status: number, message: string, limit?: LimitInfo) {
    super(message);
    this.name = "ApiRequestError";
    this.status = status;
    this.limit = limit;
  }
}

// ── Unauthorized (401) handling ──────────────────────────────────────────────
// The app registers exactly one handler (AuthProvider's Bridge → login(), which
// preserves the current path and starts the OIDC redirect). A burst of 401s —
// e.g. the 2s marketplace-status poll plus page queries all failing at once —
// must produce exactly one redirect: once fired, the guard stays closed until a
// handler is (re-)registered.
let onUnauthorized: (() => void) | null = null;
let unauthorizedFired = false;

export function setUnauthorizedHandler(fn: (() => void) | null): void {
  onUnauthorized = fn;
  unauthorizedFired = false;
}

export function notifyUnauthorized(): void {
  if (unauthorizedFired || !onUnauthorized) return;
  unauthorizedFired = true;
  onUnauthorized();
}

// ── Community-edition cap (402) handling ─────────────────────────────────────
// Same registration shape as the 401 pair above, with one deliberate
// difference: NO single-fire latch lives here. A 402 carries a payload the
// modal renders, and the burst policy (what is on screen right now, which
// resource the user has already dismissed) needs state the React layer owns.
// LimitReachedGate holds it. A latch in both places would be two policies
// fighting over one dialog.
let onLimitReached: ((info: LimitInfo) => void) | null = null;

export function setLimitReachedHandler(fn: ((info: LimitInfo) => void) | null): void {
  onLimitReached = fn;
}

export function notifyLimitReached(info: LimitInfo): void {
  onLimitReached?.(info);
}

/**
 * The Community-edition cap payload carried by `e`, or null if `e` is not a
 * cap error. The `isConflict` (pages/admin/conflict.ts) analogue for 402, and
 * the single place that classifies one.
 *
 * Unlike `isConflict`, which has three page-level callers, this currently has
 * no consumer outside `notifyIfLimitReached` below and its own tests: the
 * dialog is global, so no page classifies a 402 for itself. It is exported
 * anyway so the classification can be asserted directly, and so a future
 * page-level caller has one to use rather than spelling out its own check.
 *
 * It tests `limit` rather than the status because apiFetch attaches `limit` on
 * a 402 and on nothing else (see ApiRequestError.limit), and because a 402
 * whose body could not be parsed into a complete LimitInfo must NOT be
 * classified as a cap: the modal renders the limit and the count straight from
 * the response, so a payload we cannot read is one we may not guess at. Such a
 * 402 falls through to the ordinary error path (QueryGate's message) instead.
 *
 * Lives here, not in its own module beside the component the way conflict.ts
 * does: conflict.ts is split out because a .tsx exporting a component AND a
 * plain function trips react-refresh/only-export-components, which does not
 * apply to a .ts module, and its only caller is `notifyIfLimitReached` below
 * (reached from createAppQueryClient's two caches), so a separate module would
 * make client.ts and that module import each other.
 */
export function limitInfo(e: unknown): LimitInfo | null {
  return e instanceof ApiRequestError ? (e.limit ?? null) : null;
}

function notifyIfLimitReached(e: unknown): void {
  const info = limitInfo(e);
  if (info) notifyLimitReached(info);
}

/**
 * Structural parse of a 402 body's `limit` object. Every field is checked,
 * and a body missing one, or carrying a string where the API sends a number,
 * yields undefined rather than a half-populated object that would render as
 * "undefined of undefined used".
 */
function parseLimit(v: unknown): LimitInfo | undefined {
  if (typeof v !== "object" || v === null) return undefined;
  const { resource, max, current, contact } = v as Record<string, unknown>;
  if (typeof resource !== "string" || resource === "") return undefined;
  // Number.isInteger, not `typeof === "number"`. NaN and Infinity are the
  // headline cases but are unreachable through JSON.parse; a FRACTIONAL count
  // is not, and "2.5 of 10 used" is nonsense the dialog would render
  // faithfully. Go sends integers, so this is the shape to demand.
  if (typeof max !== "number" || typeof current !== "number") return undefined;
  // Both checks are needed: Number.isInteger is not a type guard, so the
  // typeof above is what narrows `unknown` to `number` for the return below,
  // and this is what rejects the value.
  if (!Number.isInteger(max) || !Number.isInteger(current)) return undefined;

  if (typeof contact !== "string" || contact === "") return undefined;
  return { resource, max, current, contact };
}

/** Human-readable message for a query/mutation error. */
export function errMsg(e: unknown): string {
  if (e instanceof ApiRequestError) return e.message;
  if (e) return "request failed";
  return "";
}

/**
 * The app-wide QueryClient: 4xx errors (auth/validation) are never retried —
 * retrying can't fix them and only delays surfacing the failure — while
 * 5xx/network errors get a bounded retry. Any query erroring with a 401 kicks
 * off the unauthorized flow (single-fire, see above).
 *
 * A 402 is wired on BOTH caches, and the mutation half is not the important
 * one. The seat cap (spec §3.2) is enforced in authz.Resolver.Middleware
 * (internal/authz/resolved.go), mounted after RequireAuth and therefore run on
 * EVERY authenticated request, so a capped user is refused on ordinary
 * catalog and admin GETs and never gets as far as a mutation. Wiring only the
 * mutation path would leave the most likely Community failure, a user who
 * cannot get in at all, with no dialog. The server and role caps are the
 * write-time half, and useInvalidating's mutations surface their errors only
 * to the calling component, so the MutationCache is the one place a mutation
 * error is visible app-wide.
 */
export function createAppQueryClient(): QueryClient {
  return new QueryClient({
    queryCache: new QueryCache({
      onError: (err) => {
        if (err instanceof ApiRequestError && err.status === 401) notifyUnauthorized();
        notifyIfLimitReached(err);
      },
    }),
    mutationCache: new MutationCache({
      onError: (err) => notifyIfLimitReached(err),
    }),
    defaultOptions: {
      queries: {
        retry: (count, err) =>
          !(err instanceof ApiRequestError && err.status >= 400 && err.status < 500) &&
          count < 2,
      },
    },
  });
}

interface Options {
  method?: string;
  body?: unknown;
  /** TanStack passes its queryFn AbortSignal here; combined with a hard timeout. */
  signal?: AbortSignal;
  /**
   * The optimistic-concurrency precondition (spec §5), e.g. `"3"` — quoted,
   * matching the strong entity-tag the server emits (unquoted is a 400).
   * Deliberately narrower than a general `headers` bag: exactly one call
   * path (a server/artifact PUT, or an artifact approve) ever needs to set a
   * precondition, and this keeps arbitrary headers from being smuggled
   * through apiFetch's callers.
   */
  ifMatch?: string;
}

// A request that never completes must not wedge the query forever (the browser
// analogue of the v1.16.0 orbeat-sync no-timeout hang). Every call gets a hard
// 30s ceiling, combined with any caller-supplied signal (query cancellation) so
// whichever fires first aborts the fetch.
const REQUEST_TIMEOUT_MS = 30_000;

export async function apiFetch<T>(
  path: string,
  token: string,
  opts: Options = {},
): Promise<T> {
  const timeout = AbortSignal.timeout(REQUEST_TIMEOUT_MS);
  const signal = opts.signal ? AbortSignal.any([opts.signal, timeout]) : timeout;
  const res = await fetch(`${config.apiBase}${path}`, {
    method: opts.method ?? "GET",
    headers: {
      Authorization: `Bearer ${token}`,
      ...(opts.body !== undefined ? { "Content-Type": "application/json" } : {}),
      ...(opts.ifMatch ? { "If-Match": opts.ifMatch } : {}),
    },
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    signal,
  });

  if (res.status === 204) return undefined as T;

  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    let limit: LimitInfo | undefined;
    try {
      // ONE read. A Response body is a stream that can be consumed exactly
      // once, so the message and the 402 cap payload both come out of this
      // single parse; a second res.json() anywhere below would throw on the
      // unusable body and be swallowed by the catch.
      const e = (await res.json()) as { error?: { message?: string }; limit?: unknown };
      if (e.error?.message) msg = e.error.message;
      // The portal's single copy of the cap status code (see
      // ApiRequestError.limit and limitInfo). Every other status leaves
      // `limit` undefined even if the body carries one.
      if (res.status === 402) limit = parseLimit(e.limit);
    } catch {
      // non-json error body — keep the default message
    }
    throw new ApiRequestError(res.status, msg, limit);
  }

  return (await res.json()) as T;
}
