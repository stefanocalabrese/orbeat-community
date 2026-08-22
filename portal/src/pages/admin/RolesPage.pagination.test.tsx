/**
 * Pagination append behavior for the admin Roles list (Task 10 — the API's
 * five admin lists are now cursor-paginated envelopes: `{ roles, limit,
 * nextCursor }`; the portal must fetch page 1, then append (never replace)
 * subsequent pages via a "Load more" control).
 *
 * This suite mocks `fetch`, like every other portal unit test in this repo —
 * it can only prove the CLIENT-SIDE append/Load-more logic against whatever
 * shape the mock hands back. It CANNOT prove the real API actually returns
 * this envelope shape, that `?cursor=` round-trips correctly against the
 * live store, or that the `?limit`/`?cursor` contract holds end to end. That
 * is exactly the v1.16.0 failure mode (a portal test mocking `fetch` stayed
 * green while the real feature was dead) — Task 12's Playwright e2e spec
 * against the real compose stack is the actual gate for the API↔portal seam,
 * not this file.
 */
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import RolesPage from "./RolesPage";

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
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <RolesPage />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("Load more appends the second page, keeps the first page on screen, and disappears once exhausted", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("cursor=")) {
      return Promise.resolve(
        json({ roles: [{ id: "2", name: "orbeat-admin" }], limit: 1, nextCursor: "" }),
      );
    }
    return Promise.resolve(
      json({ roles: [{ id: "1", name: "orbeat-user" }], limit: 1, nextCursor: "abc" }),
    );
  });
  renderPage();

  const list = await screen.findByRole("list");
  expect(within(list).getByText("orbeat-user")).toBeInTheDocument();

  const loadMore = screen.getByRole("button", { name: /load more/i });
  await user.click(loadMore);

  // append, not replace: the first page's row must still be present
  expect(await within(list).findByText("orbeat-admin")).toBeInTheDocument();
  expect(within(list).getByText("orbeat-user")).toBeInTheDocument();

  // the second page was the last (nextCursor == ""), so the button is gone
  expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument();
});

test("an exact-multiple-of-limit final page returns zero rows and no cursor without an empty-state flash or a stuck button", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("cursor=")) {
      return Promise.resolve(json({ roles: [], limit: 1, nextCursor: "" }));
    }
    return Promise.resolve(
      json({ roles: [{ id: "1", name: "orbeat-user" }], limit: 1, nextCursor: "abc" }),
    );
  });
  renderPage();

  const list = await screen.findByRole("list");
  await user.click(screen.getByRole("button", { name: /load more/i }));

  // the zero-row final page must not clear the list or show an empty state,
  // and the button must not get stuck rendering after exhaustion
  await vi.waitFor(() =>
    expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument(),
  );
  expect(within(list).getByText("orbeat-user")).toBeInTheDocument();
});

test("Load more is not shown when the first page is not full (no nextCursor)", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json({ roles: [{ id: "1", name: "orbeat-user" }], limit: 100, nextCursor: "" }),
  );
  renderPage();
  await screen.findByRole("list");
  expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument();
});
