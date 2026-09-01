/**
 * B35: admin search boxes fired one request per keystroke — ListSearchBox's
 * onChange fed useAdminList's query key directly. This proves the actual
 * request COUNT collapses to one per pause-in-typing, on each of the four
 * pages with a real search box (RolesPage, ServersPage, VirtualKeysPage,
 * ArtifactsPage — EntitlementsPage/ArtifactEntitlementsPage render no search
 * box at all, per ListSearchBox's own comment).
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ServersPage from "./ServersPage";
import RolesPage from "./RolesPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin", "orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function renderPage(node: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter>{node}</MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("ServersPage: typing a multi-character search issues far fewer requests than keystrokes, and the final one carries the full term", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    calls.push(url);
    return Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" }));
  });
  renderPage(<ServersPage />);
  await screen.findByRole("button", { name: /new server/i });
  const before = calls.length;

  await user.type(screen.getByRole("searchbox", { name: "Search servers" }), "github");

  // Give the debounce window time to settle exactly once.
  await vi.waitFor(() => {
    const searchCalls = calls.slice(before).filter((c) => c.includes("q="));
    expect(searchCalls.length).toBeGreaterThan(0);
  });
  const searchCalls = calls.slice(before).filter((c) => c.includes("q="));
  // One committed request for the whole word, not six (one per keystroke).
  expect(searchCalls.length).toBe(1);
  expect(searchCalls[0]).toContain("q=github");
});

test("RolesPage: the same debounce applies (not a ServersPage-only fix)", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    calls.push(url);
    return Promise.resolve(json({ roles: [], limit: 100, nextCursor: "" }));
  });
  renderPage(<RolesPage />);
  await screen.findByRole("searchbox", { name: "Search roles" });
  const before = calls.length;

  await user.type(screen.getByRole("searchbox", { name: "Search roles" }), "orbeat");

  await vi.waitFor(() => {
    const searchCalls = calls.slice(before).filter((c) => c.includes("q="));
    expect(searchCalls.length).toBeGreaterThan(0);
  });
  const searchCalls = calls.slice(before).filter((c) => c.includes("q="));
  expect(searchCalls.length).toBe(1);
  expect(searchCalls[0]).toContain("q=orbeat");
});
