/**
 * The 401 re-login path, driven through the MUTATION cache.
 *
 * Split into its own file rather than added to client.test.ts because driving
 * a mutation needs a QueryClientProvider and therefore JSX, which a .ts file
 * cannot carry, the same reason client.limit.test.tsx exists beside it.
 *
 * WHY THIS FILE EXISTS AT ALL. Every 401 assertion in this repo used to drive
 * a query: client.test.ts's concurrent-401 test calls fetchQuery,
 * AuthProvider.test.tsx calls notifyUnauthorized() directly, and
 * e2e/expired-session.spec.ts routes every admin call to 401 and then only
 * reloads the page, which issues GETs. So MutationCache.onError could call
 * notifyIfLimitReached alone, with no 401 arm at all, and the whole suite
 * stayed green while an admin whose session expired mid-form got an inline
 * "HTTP 401" on Create and nothing else. Only ArtifactsPage polls, so on
 * Roles, Servers, Entitlements and Virtual keys nothing else would ever fail
 * to reveal it.
 *
 * fetch is mocked here, so this proves the client ROUTES a 401 from either
 * cache into the single-fire handler. It does not prove the server sends one;
 * e2e/expired-session.spec.ts owns that half in a real browser.
 */
import type { ReactNode } from "react";
import { QueryClientProvider, useMutation } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  apiFetch,
  createAppQueryClient,
  setUnauthorizedHandler,
} from "./client";
import type { QueryClient } from "@tanstack/react-query";

const unauthorized = () =>
  new Response(JSON.stringify({ error: { message: "session expired" } }), {
    status: 401,
    headers: { "Content-Type": "application/json" },
  });

beforeEach(() => vi.restoreAllMocks());
// Also re-arms the single-fire latch for the next test (setUnauthorizedHandler
// resets it), so these tests cannot leak a fired latch into each other.
afterEach(() => setUnauthorizedHandler(null));

/** Renders a useMutation POST against qc and runs it to completion. */
function renderFailingMutation(qc: QueryClient, path: string) {
  function wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  }
  return renderHook(
    () =>
      useMutation({
        mutationFn: () => apiFetch(path, "t", { method: "POST", body: {} }),
        retry: false,
      }),
    { wrapper },
  );
}

test("a 401 failing MUTATION fires the unauthorized handler", async () => {
  const handler = vi.fn();
  setUnauthorizedHandler(handler);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(unauthorized());

  const qc = createAppQueryClient();
  const { result } = renderFailingMutation(qc, "/v1/admin/artifacts");
  act(() => result.current.mutate());
  await waitFor(() => expect(result.current.isError).toBe(true));

  expect(handler).toHaveBeenCalledTimes(1);
});

test("a burst of 401 mutations still fires the handler exactly once", async () => {
  const handler = vi.fn();
  setUnauthorizedHandler(handler);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(unauthorized());

  const qc = createAppQueryClient();
  // Three separate mutations, as an admin retrying Create would produce. One
  // overlay, not one per failed write.
  for (const path of ["/v1/admin/roles", "/v1/admin/servers", "/v1/admin/roles"]) {
    const { result } = renderFailingMutation(qc, path);
    act(() => result.current.mutate());
    await waitFor(() => expect(result.current.isError).toBe(true));
  }

  expect(handler).toHaveBeenCalledTimes(1);
});

test("a mutation 401 and a query 401 do not double-fire the handler", async () => {
  const handler = vi.fn();
  setUnauthorizedHandler(handler);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(unauthorized());

  const qc = createAppQueryClient();
  const { result } = renderFailingMutation(qc, "/v1/admin/servers");
  act(() => result.current.mutate());
  await waitFor(() => expect(result.current.isError).toBe(true));
  // Checked HERE, before the query runs, and this ordering is load-bearing:
  // asserting only the final total of 1 passes on the pre-fix code too, since
  // the query alone would supply that one call. The mutation must be the one
  // that fired it.
  expect(handler).toHaveBeenCalledTimes(1);

  // The page's own list query fails next, on the same dead token. The latch is
  // one latch across BOTH caches, not one per cache.
  await qc
    .fetchQuery({ queryKey: ["servers"], queryFn: () => apiFetch("/v1/admin/servers", "t") })
    .catch(() => {});

  expect(handler).toHaveBeenCalledTimes(1);
});

test("a non-401 mutation failure fires nothing", async () => {
  const handler = vi.fn();
  setUnauthorizedHandler(handler);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ error: { message: "already exists" } }), {
      status: 409,
      headers: { "Content-Type": "application/json" },
    }),
  );

  const qc = createAppQueryClient();
  const { result } = renderFailingMutation(qc, "/v1/admin/servers");
  act(() => result.current.mutate());
  await waitFor(() => expect(result.current.isError).toBe(true));

  expect(handler).not.toHaveBeenCalled();
});
