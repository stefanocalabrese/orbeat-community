import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import AuditPage from "./AuditPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { "Content-Type": "application/json" } });
const ev = (id: string, actor: string, action: string, decision = "allow") => ({
  id, ts: "2026-06-11T10:00:00Z", actor, action, target: "x", decision, metadata: {},
});

let urls: string[] = [];

/**
 * The server is mocked to actually honour ?actor=, so a page that fetches the
 * right URL and a page that ignores the response render differently. A mock
 * returning one fixed list would let every assertion below pass on a filter
 * wired to nothing.
 */
beforeEach(() => {
  urls = [];
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    urls.push(url);
    const q = new URLSearchParams(url.split("?")[1] ?? "");
    const all = [ev("1", "alice", "role.delete", "deny"), ev("2", "bob", "artifact.approve")];
    const matching = all.filter((e) => (!q.get("actor") || e.actor === q.get("actor")) && (!q.get("decision") || e.decision === q.get("decision")));
    const page = q.get("cursor") ? matching.slice(1) : matching.slice(0, 1);
    return Promise.resolve(json({ events: page, limit: 50, nextCursor: q.get("cursor") ? "" : "next" }));
  });
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><AuditPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
}

/**
 * The decisive one, and the reason it is written with DISJOINT sets: the
 * unfiltered first page shows only bob, the filtered one only alice. An
 * assertion built on a row that appears in both results (as an earlier draft of
 * the test above was) passes just as well when the page keeps rendering the
 * pre-filter response, because the row it looks for is in both. Here the two
 * pages have nothing in common, so "shows alice, does not show bob" can only be
 * satisfied by actually consuming the filtered response.
 */
test("applying a filter renders the filtered page, not the one it replaced", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    urls.push(url);
    const q = new URLSearchParams(url.split("?")[1] ?? "");
    const events = q.get("actor") === "alice"
      ? [ev("1", "alice", "alice.did.this")]
      : [ev("2", "bob", "bob.did.this")];
    return Promise.resolve(json({ events, limit: 50, nextCursor: "" }));
  });
  renderPage();
  expect(await screen.findByText("bob.did.this")).toBeInTheDocument();

  await user.type(screen.getByLabelText("Filter by actor"), "alice");
  await user.click(screen.getByRole("button", { name: /apply filters/i }));

  expect(await screen.findByText("alice.did.this")).toBeInTheDocument();
  expect(screen.queryByText("bob.did.this")).not.toBeInTheDocument();
});

test("applying an actor filter refetches and REPLACES the accumulated rows", async () => {
  const user = userEvent.setup();
  renderPage();
  expect(await screen.findByText("role.delete")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /load more/i }));
  expect(await screen.findByText("artifact.approve")).toBeInTheDocument();

  await user.type(screen.getByLabelText("Filter by actor"), "alice");
  await user.click(screen.getByRole("button", { name: /apply filters/i }));

  await waitFor(() => expect(urls.some((u) => u.includes("actor=alice"))).toBe(true));
  // bob's row came from the pre-filter pages. If applying a filter only reset
  // the cursor, it would still be on screen under the new filter.
  await waitFor(() => expect(screen.queryByText("artifact.approve")).not.toBeInTheDocument());
  expect(screen.getByText("role.delete")).toBeInTheDocument();
});

test("Load more keeps the filters, because the cursor encodes a position and not a query", async () => {
  const user = userEvent.setup();
  renderPage();
  await screen.findByText("role.delete");

  await user.selectOptions(screen.getByLabelText("Filter by decision"), "deny");
  await user.click(screen.getByRole("button", { name: /apply filters/i }));
  await waitFor(() => expect(urls.some((u) => u.includes("decision=deny"))).toBe(true));

  const before = urls.length;
  await user.click(await screen.findByRole("button", { name: /load more/i }));
  await waitFor(() => expect(urls.length).toBeGreaterThan(before));
  const paged = urls.slice(before).filter((u) => u.includes("cursor="));
  expect(paged.length).toBeGreaterThan(0);
  for (const u of paged) expect(u).toContain("decision=deny");
});

test("Clear discards a typed draft instead of submitting it", async () => {
  const user = userEvent.setup();
  renderPage();
  await screen.findByText("role.delete");

  await user.type(screen.getByLabelText("Filter by actor"), "alice");
  const before = urls.length;
  // Button renders a bare <button>, which inside a form defaults to
  // type="submit": an untyped Clear would fetch the very draft it discards.
  await user.click(screen.getByRole("button", { name: /^clear$/i }));
  await waitFor(() => expect(screen.getByLabelText("Filter by actor")).toHaveValue(""));
  expect(urls.slice(before).some((u) => u.includes("actor=alice"))).toBe(false);
});

test("an unfiltered request sends no filter parameters at all", async () => {
  renderPage();
  await screen.findByText("role.delete");
  for (const u of urls) {
    expect(u).not.toContain("actor=");
    expect(u).not.toContain("action=");
    expect(u).not.toContain("decision=");
  }
});
