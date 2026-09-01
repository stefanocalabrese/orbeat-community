/**
 * Task 5 (docs/plans/orbeat-admin-search-sort-2026-08-27.md): sortable column
 * headers + a search box on the admin tables, driven through the real pages
 * rather than the bare hooks (queries.sortsearch.test.tsx covers the hook
 * contract in isolation).
 *
 * The load-bearing requirement is dropping any outstanding cursor on a sort,
 * direction or search change: the API binds a cursor to the sort identity
 * it was minted under and 400s a replay under a different one (8935bb9,
 * 8e0636c, 1e2cffa). Every test here that accumulates a cursor (via "Load
 * more") and then changes a control asserts the FOLLOWING request carries no
 * `cursor=`.
 *
 * Mocks `fetch`, like every other portal unit test in this repo. It proves
 * the CLIENT wires header clicks and the search box to the right request,
 * nothing about whether the real API accepts it end to end. That is
 * portal/e2e/adminSortSearch.spec.ts, run against the real compose stack.
 */
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ServersPage from "./ServersPage";
import RolesPage from "./RolesPage";
import ArtifactsPage from "./ArtifactsPage";
import VirtualKeysPage from "./VirtualKeysPage";
import EntitlementsPage from "./EntitlementsPage";
import ArtifactEntitlementsPage from "./ArtifactEntitlementsPage";

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

function urlOf(input: RequestInfo | URL): string {
  return String(input instanceof Request ? input.url : input);
}

function renderPage(node: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>{node}</MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

// ── ServersPage ─────────────────────────────────────────────────────────────

test("ServersPage: clicking the Name header sorts descending and drops the cursor; a second click flips back", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.includes("order=desc")) {
      return Promise.resolve(json({ servers: [server("2", "zeta")], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ servers: [server("1", "alpha")], limit: 100, nextCursor: "" }));
  });
  renderPage(<ServersPage />);
  expect(await screen.findByText("alpha")).toBeInTheDocument();
  expect(calls).toHaveLength(1);
  expect(calls[0]).not.toContain("cursor=");

  // every OTHER header stays a plain, non-interactive <th>: the API allows
  // sorting on exactly one column per list, so a clickable-looking header
  // that does nothing would be worse than one that plainly is not.
  expect(screen.queryByRole("button", { name: "Transport" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Status" })).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Name" }));

  expect(await screen.findByText("zeta")).toBeInTheDocument();
  expect(screen.queryByText("alpha")).not.toBeInTheDocument();
  // exactly one new request for the click, not zero and not two
  expect(calls).toHaveLength(2);
  expect(calls[1]).toContain("order=desc");
  expect(calls[1]).not.toContain("cursor=");

  await user.click(screen.getByRole("button", { name: "Name" }));
  await screen.findByText("alpha");
  expect(calls).toHaveLength(3);
  expect(calls[2]).not.toContain("order=desc");
});

test("ServersPage: searching issues ?q= with no cursor, and changing the search again resets paging again", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.includes("cursor=")) {
      // page 2 of the FIRST (unfiltered) search: only reachable if the
      // cursor were wrongly carried into a later request.
      return Promise.resolve(json({ servers: [server("2", "bravo")], limit: 1, nextCursor: "" }));
    }
    if (url.includes("q=lab")) {
      return Promise.resolve(json({ servers: [server("3", "gitlab")], limit: 1, nextCursor: "" }));
    }
    if (url.includes("q=git")) {
      return Promise.resolve(json({ servers: [server("1", "github")], limit: 1, nextCursor: "abc" }));
    }
    return Promise.resolve(json({ servers: [server("1", "github")], limit: 1, nextCursor: "abc" }));
  });
  renderPage(<ServersPage />);
  await screen.findByText("github");

  await user.type(screen.getByRole("searchbox", { name: "Search servers" }), "git");
  // B35: search is now debounced (300ms) — wait for the actual debounced
  // request rather than jump straight to `findByText("github")`, since the
  // fixture's UNFILTERED default page also renders "github" and would
  // otherwise let this assertion pass on the stale pre-debounce request.
  await vi.waitFor(() => expect(calls.at(-1)).toContain("q=git"));
  await screen.findByText("github");
  const afterFirstSearch = calls.length;
  expect(calls[afterFirstSearch - 1]).toContain("q=git");
  expect(calls[afterFirstSearch - 1]).not.toContain("cursor=");

  // accumulate a cursor under this search term
  await user.click(await screen.findByRole("button", { name: /load more/i }));
  await screen.findByText("bravo");
  expect(calls.at(-1)).toContain("cursor=");

  // changing the search term again must drop that cursor, not replay it
  await user.clear(screen.getByRole("searchbox", { name: "Search servers" }));
  await user.type(screen.getByRole("searchbox", { name: "Search servers" }), "lab");
  await vi.waitFor(() => expect(calls.at(-1)).toContain("q=lab"));
  await screen.findByText("gitlab");
  expect(screen.queryByText("bravo")).not.toBeInTheDocument();
  expect(calls.at(-1)).toContain("q=lab");
  expect(calls.at(-1)).not.toContain("cursor=");
});

function server(id: string, name: string) {
  return {
    id,
    name,
    description: "",
    transport: "http",
    endpointOrCommand: "https://x",
    version: "",
    protocolVersion: "",
    status: "active",
    hasSecret: false,
    rowVersion: 1,
  };
}

// ── RolesPage ───────────────────────────────────────────────────────────────

test("RolesPage: clicking the Name header sorts descending with no cursor", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.includes("order=desc")) {
      return Promise.resolve(json({ roles: [{ id: "2", name: "zeta", rowVersion: 1 }], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ roles: [{ id: "1", name: "alpha", rowVersion: 1 }], limit: 100, nextCursor: "" }));
  });
  renderPage(<RolesPage />);
  await screen.findByText("alpha");

  await user.click(screen.getByRole("button", { name: "Name" }));

  await screen.findByText("zeta");
  expect(calls).toHaveLength(2);
  expect(calls[1]).toContain("order=desc");
  expect(calls[1]).not.toContain("cursor=");
});

test("RolesPage: searching issues ?q= with no cursor", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.includes("q=orbeat")) {
      return Promise.resolve(json({ roles: [{ id: "1", name: "orbeat-user", rowVersion: 1 }], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ roles: [], limit: 100, nextCursor: "" }));
  });
  renderPage(<RolesPage />);
  await screen.findByRole("list");

  await user.type(screen.getByRole("searchbox", { name: "Search roles" }), "orbeat");

  // Re-queried AFTER typing settles, not captured beforehand: every
  // intermediate keystroke is itself a distinct, empty-result query
  // (RolesPage renders the empty list through QueryGate, which unmounts and
  // remounts the <ul> on each one), so a <ul> reference captured before
  // typing is stale by the time the final "orbeat" result lands. Scoped to
  // the list itself: the page's own explanatory copy above it also mentions
  // "orbeat-user" as an example role name.
  await waitFor(async () => {
    within(await screen.findByRole("list")).getByText("orbeat-user");
  });
  expect(calls.at(-1)).toContain("q=orbeat");
  expect(calls.at(-1)).not.toContain("cursor=");
});

// ── ArtifactsPage ───────────────────────────────────────────────────────────

const publishStatus = { lastAttemptAt: null, lastSuccessAt: null, lastCommit: "abc123", lastError: "" };

function artifact(id: string, name: string) {
  return {
    id,
    type: "skill" as const,
    name,
    description: "",
    content: "body",
    memoryScope: null,
    memorySeed: null,
    version: "0.1.0",
    approvalState: "approved" as const,
    approved: true,
    visibility: "org" as const,
  };
}

test("ArtifactsPage: clicking the Type header sorts descending with no cursor (the sortable column is Type, not Name)", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("order=desc")) {
      return Promise.resolve(json({ artifacts: [artifact("2", "zeta")], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [artifact("1", "alpha")], limit: 100, nextCursor: "" }));
  });
  renderPage(<ArtifactsPage />);
  await screen.findByText("alpha");

  await user.click(screen.getByRole("button", { name: "Type" }));

  await screen.findByText("zeta");
  const listCalls = calls.filter((c) => c.includes("/v1/admin/artifacts") && !c.includes("/marketplace/status"));
  expect(listCalls).toHaveLength(2);
  expect(listCalls[1]).toContain("order=desc");
  expect(listCalls[1]).not.toContain("cursor=");
});

test("ArtifactsPage: searching issues ?q= against name with no cursor", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("q=fmt")) {
      return Promise.resolve(json({ artifacts: [artifact("1", "fmt")], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage(<ArtifactsPage />);
  await screen.findByRole("button", { name: /new artifact/i });

  await user.type(screen.getByRole("searchbox", { name: "Search artifacts" }), "fmt");

  await screen.findByText("fmt");
  const listCalls = calls.filter((c) => c.includes("/v1/admin/artifacts") && !c.includes("/marketplace/status"));
  expect(listCalls.at(-1)).toContain("q=fmt");
  expect(listCalls.at(-1)).not.toContain("cursor=");
});

// ── VirtualKeysPage ─────────────────────────────────────────────────────────

const meOn = { subject: "boss", email: "b@x", roles: ["orbeat-admin"], features: { pinning: true, virtualKeys: true } };

function vkey(id: string, name: string) {
  return { id, clientId: `client-${id}`, roleId: "role-1", name, description: "", revoked: false, createdAt: "2026-01-01T00:00:00Z", rowVersion: 1 };
}

test("VirtualKeysPage: clicking the Name header sorts descending with no cursor", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/v1/admin/roles")) {
      return Promise.resolve(json({ roles: [{ id: "role-1", name: "ops", rowVersion: 1 }], limit: 100, nextCursor: "" }));
    }
    if (url.includes("order=desc")) {
      return Promise.resolve(json({ virtualKeys: [vkey("2", "zeta")], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ virtualKeys: [vkey("1", "alpha")], limit: 100, nextCursor: "" }));
  });
  renderPage(<VirtualKeysPage />);
  await screen.findByText("alpha");

  await user.click(screen.getByRole("button", { name: "Name" }));

  await screen.findByText("zeta");
  const listCalls = calls.filter((c) => c.includes("/v1/admin/virtual-keys"));
  expect(listCalls).toHaveLength(2);
  expect(listCalls[1]).toContain("order=desc");
  expect(listCalls[1]).not.toContain("cursor=");
});

test("VirtualKeysPage: searching issues ?q= with no cursor", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/v1/admin/roles")) {
      return Promise.resolve(json({ roles: [{ id: "role-1", name: "ops", rowVersion: 1 }], limit: 100, nextCursor: "" }));
    }
    if (url.includes("q=bot")) {
      return Promise.resolve(json({ virtualKeys: [vkey("1", "ci-bot")], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ virtualKeys: [], limit: 100, nextCursor: "" }));
  });
  renderPage(<VirtualKeysPage />);
  await screen.findByRole("button", { name: /new virtual key/i });

  await user.type(screen.getByRole("searchbox", { name: "Search virtual keys" }), "bot");

  await screen.findByText("ci-bot");
  const listCalls = calls.filter((c) => c.includes("/v1/admin/virtual-keys"));
  expect(listCalls.at(-1)).toContain("q=bot");
  expect(listCalls.at(-1)).not.toContain("cursor=");
});

// ── EntitlementsPage / ArtifactEntitlementsPage: sortable, but NEVER q ──────

test("EntitlementsPage: the Role header sorts descending with no cursor, and no search box is rendered", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user", rowVersion: 1 }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/servers")) return Promise.resolve(json({ servers: [{ id: "s1", name: "github", description: "", transport: "http", endpointOrCommand: "x", version: "", protocolVersion: "", status: "active", hasSecret: false, rowVersion: 1 }], limit: 100, nextCursor: "" }));
    if (url.includes("order=desc")) {
      return Promise.resolve(json({ entitlements: [{ id: "e2", roleId: "r1", mcpServerId: "s1", allowedTools: null, permissions: [], rowVersion: 1 }], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ entitlements: [{ id: "e1", roleId: "r1", mcpServerId: "s1", allowedTools: ["echo"], permissions: [], rowVersion: 1 }], limit: 100, nextCursor: "" }));
  });
  renderPage(<EntitlementsPage />);
  await screen.findByText(/echo/);

  expect(screen.queryByRole("searchbox")).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Role" }));

  await waitFor(() => expect(screen.queryByText(/echo/)).not.toBeInTheDocument());
  const entitlementCalls = calls.filter((c) => c.includes("/v1/admin/entitlements"));
  expect(entitlementCalls).toHaveLength(2);
  expect(entitlementCalls[1]).toContain("order=desc");
  expect(entitlementCalls[1]).not.toContain("cursor=");
  expect(entitlementCalls.every((c) => !c.includes("q="))).toBe(true);
});

test("ArtifactEntitlementsPage: the Role header sorts descending with no cursor, and no search box is rendered", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = urlOf(input);
    calls.push(url);
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user", rowVersion: 1 }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/artifacts")) return Promise.resolve(json({ artifacts: [{ ...artifact("a1", "seed"), visibility: "role" as const }], limit: 100, nextCursor: "" }));
    if (url.includes("order=desc")) {
      return Promise.resolve(json({ artifactEntitlements: [{ id: "ae2", roleId: "r1", artifactId: "a1" }], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifactEntitlements: [{ id: "ae1", roleId: "r1", artifactId: "a1" }], limit: 100, nextCursor: "" }));
  });
  renderPage(<ArtifactEntitlementsPage />);
  await screen.findByText("seed");

  expect(screen.queryByRole("searchbox")).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Role" }));

  await waitFor(() => {
    const entitlementCalls = calls.filter((c) => c.includes("/v1/admin/artifact-entitlements"));
    expect(entitlementCalls).toHaveLength(2);
  });
  const entitlementCalls = calls.filter((c) => c.includes("/v1/admin/artifact-entitlements"));
  expect(entitlementCalls[1]).toContain("order=desc");
  expect(entitlementCalls[1]).not.toContain("cursor=");
  expect(entitlementCalls.every((c) => !c.includes("q="))).toBe(true);
});
