/**
 * Task 5 (docs/plans/orbeat-admin-search-sort-2026-08-27.md): portal side of
 * ?sort/?order/?q. The API (8935bb9, 8e0636c, 1e2cffa) binds a cursor to the
 * sort identity it was minted under and 400s on a mismatch, so THE load-
 * bearing behavior here is that changing order or q must never let a
 * previously-issued cursor be replayed under the new one.
 *
 * `useAdminList` achieves this by folding order/q into the query's own
 * `queryKey` (see queries.ts), not just into the request URL: a distinct
 * (order, q) pair is a distinct key, so react-query starts it fresh at page
 * one instead of resuming the OLD key's accumulated cursor. This file mocks
 * `fetch`, like every other portal unit test in this repo. It proves the
 * CLIENT drops the cursor and builds the right URL, nothing about whether
 * the real API accepts it end to end (that is the Playwright spec).
 */
import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, expect, test, vi } from "vitest";
import { AuthCtx } from "../auth/useAuth";
import { useAdminServers, useEntitlements, useRoles } from "./queries";

const boss = {
  isLoading: false,
  authenticated: true,
  roles: ["orbeat-admin", "orbeat-user"],
  token: "t",
  login: () => {},
  logout: () => {},
  subject: "boss",
  email: "b@x",
};

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return (
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    </AuthCtx.Provider>
  );
}

beforeEach(() => vi.restoreAllMocks());

test("THE MUTANT THAT MATTERS: switching order drops the outstanding cursor instead of replaying it", async () => {
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    calls.push(url);
    if (url.includes("cursor=")) {
      return Promise.resolve(json({ roles: [{ id: "2", name: "b", rowVersion: 1 }], limit: 1, nextCursor: "" }));
    }
    if (url.includes("order=desc")) {
      return Promise.resolve(json({ roles: [{ id: "3", name: "c", rowVersion: 1 }], limit: 1, nextCursor: "" }));
    }
    return Promise.resolve(json({ roles: [{ id: "1", name: "a", rowVersion: 1 }], limit: 1, nextCursor: "abc" }));
  });

  const { result, rerender } = renderHook(({ order }: { order: "asc" | "desc" }) => useRoles({ order }), {
    wrapper,
    initialProps: { order: "asc" } as { order: "asc" | "desc" },
  });

  await waitFor(() => expect(result.current.hasNextPage).toBe(true));
  expect(calls).toHaveLength(1);
  expect(calls[0]).not.toContain("cursor=");

  await act(async () => {
    await result.current.fetchNextPage();
  });
  expect(calls).toHaveLength(2);
  expect(calls[1]).toContain("cursor=abc");

  rerender({ order: "desc" });

  // The correct implementation fetches page ONE of the new (order=desc) key
  // automatically, without any explicit fetchNextPage call: that IS the
  // "cursor dropped" behavior. If order/q were left out of the queryKey (the
  // mutant), react-query would see an UNCHANGED key here and never issue a
  // third request at all: this waitFor would time out and the test goes red
  // for exactly the reason this test exists.
  await waitFor(() => expect(calls.length).toBeGreaterThanOrEqual(3));
  const third = calls[2];
  if (!third) throw new Error("expected a third fetch call");
  expect(third).toContain("order=desc");
  expect(third).not.toContain("cursor=");
});

test("useAdminServers sends ?order=desc and no ?sort (the list has exactly one allowed column)", async () => {
  const spy = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(json({ servers: [], limit: 100, nextCursor: "" }));
  renderHook(() => useAdminServers({ order: "desc" }), { wrapper });

  await waitFor(() => expect(spy).toHaveBeenCalled());
  const url = String(spy.mock.calls[0]?.[0]);
  expect(url).toContain("order=desc");
  expect(url).not.toContain("sort=");
  expect(url).not.toContain("cursor=");
});

test("useAdminServers omits ?order entirely for the default ascending direction", async () => {
  const spy = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(json({ servers: [], limit: 100, nextCursor: "" }));
  renderHook(() => useAdminServers({}), { wrapper });

  await waitFor(() => expect(spy).toHaveBeenCalled());
  expect(String(spy.mock.calls[0]?.[0])).not.toContain("order=");
});

test("useAdminServers's q reaches the request untouched and changing it fetches a fresh page one", async () => {
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    calls.push(String(input));
    return Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" }));
  });

  const { rerender } = renderHook(({ q }: { q: string }) => useAdminServers({ q }), {
    wrapper,
    initialProps: { q: "git" },
  });
  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0]).toContain("q=git");

  rerender({ q: "lab" });
  await waitFor(() => expect(calls).toHaveLength(2));
  expect(calls[1]).toContain("q=lab");
  expect(calls[1]).not.toContain("cursor=");
});

test("useEntitlements's params type carries no q field at all: a compile-time guarantee, never invoked", () => {
  // @ts-expect-error entitlements REFUSE ?q= with 400 on mere presence
  // (internal/api/paging.go's refuseSearch); useEntitlements's params type
  // has no q field, so assigning one here is a compile error. Assigned to a
  // typed variable rather than calling the hook: calling it outside a
  // component body would violate the rules of hooks for no benefit. The
  // guarantee this test wants is compile-time only.
  const badParams: Parameters<typeof useEntitlements>[0] = { q: "nope" };
  void badParams;
});

test("useEntitlements's order toggle never smuggles in a q parameter", async () => {
  const spy = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(json({ entitlements: [], limit: 100, nextCursor: "" }));
  renderHook(() => useEntitlements({ order: "desc" }), { wrapper });

  await waitFor(() => expect(spy).toHaveBeenCalled());
  const url = String(spy.mock.calls[0]?.[0]);
  expect(url).toContain("order=desc");
  expect(url).not.toContain("q=");
});
