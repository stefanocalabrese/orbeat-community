/**
 * The identity diff on a review card (migration 00016).
 *
 * The card header renders the live type/name/visibility with nothing marking
 * them as a proposal. Since identity is snapshotted at approval, approving a
 * renamed artifact MOVES its file on every machine that receives it, and
 * approving a visibility flip changes which channel it arrives on. Neither is
 * visible in the content diff the reviewer is looking at.
 *
 * The fixture is ASYMMETRIC in all three fields, with values that cannot be
 * mistaken for each other: with `foo`/`foo` a diff rendering the same side
 * twice reads correctly and proves nothing.
 */
import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ReviewQueuePage from "./ReviewQueuePage";

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
const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const pending = {
  id: "p1",
  type: "subagent",
  name: "bar",
  description: "d",
  content: "proposed body",
  memoryScope: null,
  memorySeed: null,
  version: "1.0.0",
  visibility: "role",
  approvalState: "pending",
  approved: true,
  approvedContent: "live body",
  approvedType: "skill",
  approvedName: "foo",
  approvedVisibility: "org",
  submittedBy: "alice",
  rowVersion: 3,
};

function renderPage(rows: unknown[]) {
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(json({ artifacts: rows, limit: 50, nextCursor: "" })),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <ReviewQueuePage />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("a card whose identity changed shows the old-to-new pair for every changed field", async () => {
  renderPage([pending]);
  await screen.findByText("bar");

  const note = screen.getByRole("note");
  expect(within(note).getByText("skill → subagent")).toBeInTheDocument();
  expect(within(note).getByText("foo → bar")).toBeInTheDocument();
  expect(within(note).getByText("org → role")).toBeInTheDocument();
  expect(note.textContent).toContain("changes what every machine receives");
});

test("no identity diff when the proposal keeps the distributed identity", async () => {
  // Content-only change: the reviewer is approving bytes, not a path.
  renderPage([{ ...pending, type: "skill", name: "foo", visibility: "org" }]);
  await screen.findByText("foo");

  expect(screen.queryByRole("note")).toBeNull();
});

test("no identity diff on a first approval: there is no distributed identity to differ from", async () => {
  renderPage([
    {
      ...pending,
      approved: false,
      approvedContent: undefined,
      approvedType: undefined,
      approvedName: undefined,
      approvedVisibility: undefined,
    },
  ]);
  await screen.findByText("bar");

  expect(screen.queryByRole("note")).toBeNull();
});
