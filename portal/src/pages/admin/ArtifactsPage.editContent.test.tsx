/**
 * Task 11 / C8 #2 (most severe defect) + Task 11 review Important #1.
 *
 * Before this fix, ArtifactsPage's edit form prefilled directly from the
 * LIST row (`mode.content` / `mode.memorySeed`). Task 8 made
 * GET /v1/admin/artifacts slim by default: list rows carry `content: ""`
 * and `memorySeed: ""` unless `?include=content` is set. `f-content` is
 * `required`, so a blank content is noticeable — but `f-seed` is NOT
 * required, and `store.UpdateArtifact` is a full replace. An admin who
 * opened Edit, changed only e.g. the version, and saved would silently WIPE
 * an existing memorySeed they never saw — pinned server-side by
 * TestArtifactUpdateMemorySeedFullReplaceClears.
 *
 * The fix: the edit form fetches GET /v1/admin/artifacts/{id} — which
 * always carries the full payload — and prefills from THAT response, never
 * from the list row. The first test asserts exactly that source, and is
 * red-proven by reverting the prefill to the list row: the seed/content
 * assertions then fail.
 *
 * The review then found the by-id fetch had opened a narrower door of the
 * same silent-data-loss kind: the ORIGINAL render order checked `isError`
 * before `data`, so any FAILED background revalidation of an already-loaded
 * form (e.g. a window refocus, or here a manually forced refetch) replaced
 * a live, possibly mid-edit form with an error panel — discarding whatever
 * the admin had typed. The second test below guards that (Important #1),
 * red-proven by reverting ArtifactEditForm's render order back to
 * isPending/isError-before-data.
 *
 * Mocks fetch, like every other portal unit test in this repo — it cannot
 * prove the real API actually serves a slim list row alongside a full
 * by-id row over the wire, or that a real window-focus event fires a
 * refetch. That seam is Task 12's Playwright e2e against the real compose
 * stack, not this file (see C7).
 */
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ArtifactsPage from "./ArtifactsPage";

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

const publishStatus = { lastAttemptAt: null, lastSuccessAt: null, lastCommit: "abc123", lastError: "" };

// The slim LIST row (Task 8 default): content/memorySeed are present as
// empty strings, not omitted — that shape is exactly what makes the defect
// silent instead of a visible type/parse error.
const slimListRow = {
  id: "9",
  type: "subagent" as const,
  name: "rev",
  description: "d",
  content: "",
  memoryScope: "user",
  memorySeed: "",
  version: "1.0.0",
  approvalState: "approved" as const,
  approved: true,
  visibility: "role" as const,
};

// The full BY-ID row — what GET /v1/admin/artifacts/9 actually returns.
const fullArtifact = {
  ...slimListRow,
  content: "FULL CONTENT FROM BY-ID FETCH",
  memorySeed: "FULL SEED FROM BY-ID FETCH",
};

// `byId` handles GET /v1/admin/artifacts/9 specifically — defaulted to
// always returning the full artifact, overridable per test (e.g. to fail
// on the SECOND call, simulating a background revalidation gone bad).
function renderPage(byId: (callNum: number) => Response = () => json(fullArtifact)) {
  const calls: string[] = [];
  let byIdCalls = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    calls.push(url);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "GET" && url.endsWith("/v1/admin/artifacts/9")) {
      byIdCalls++;
      return Promise.resolve(byId(byIdCalls));
    }
    return Promise.resolve(json({ artifacts: [slimListRow], limit: 100, nextCursor: "" }));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const utils = render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <ArtifactsPage />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
  return { ...utils, calls, qc };
}

beforeEach(() => vi.restoreAllMocks());

test("edit form prefills content and memorySeed from GET /{id}, not the slim list row", async () => {
  const user = userEvent.setup();
  const { calls } = renderPage();

  await user.click(await screen.findByRole("button", { name: /^edit rev$/i }));

  const contentField = (await screen.findByLabelText(/content/i)) as HTMLTextAreaElement;
  expect(contentField.value).toBe(fullArtifact.content);

  const seedField = screen.getByLabelText(/seed memory/i) as HTMLTextAreaElement;
  expect(seedField.value).toBe(fullArtifact.memorySeed);

  // The decisive assertion: both differ from the slim list row's values.
  // If the form were still prefilling from the list row, these would be "".
  expect(contentField.value).not.toBe(slimListRow.content);
  expect(seedField.value).not.toBe(slimListRow.memorySeed);

  // A dedicated by-id request actually happened — not just a coincidental
  // value match.
  expect(calls.some((u) => u.endsWith("/v1/admin/artifacts/9"))).toBe(true);
});

test("a failed background refetch does not discard an in-progress edit (Important #1)", async () => {
  const user = userEvent.setup();
  const { qc } = renderPage((callNum) =>
    callNum === 1 ? json(fullArtifact) : json({ error: { message: "boom" } }, 500),
  );

  await user.click(await screen.findByRole("button", { name: /^edit rev$/i }));
  const contentField = (await screen.findByLabelText(/content/i)) as HTMLTextAreaElement;
  await user.clear(contentField);
  await user.type(contentField, "IMPORTANT UNSAVED WORK");
  expect(contentField).toHaveValue("IMPORTANT UNSAVED WORK");

  // Force a background revalidation of the SAME query — in production this
  // is a window refocus; here it is driven directly so the test does not
  // depend on refetchOnWindowFocus being on or off. This one fails.
  await qc.refetchQueries({ queryKey: ["admin", "artifacts", "byId", "9"] });
  // refetchQueries() settles once the fetch finishes, but React's re-render
  // in response to the observer's new state is not guaranteed to have
  // flushed by then. testing-library's `waitFor` (NOT vitest's `vi.waitFor`,
  // which has no knowledge of React and would happily read the still-stale
  // DOM on its very first poll and return immediately) wraps each poll in
  // `act()`, which is what actually flushes the pending render — without
  // this, the assertions below ran before the failure ever reached the
  // component and passed for the wrong reason on BOTH the fixed and the
  // broken code (caught by the red-proof itself).
  await waitFor(() => {
    expect(qc.getQueryState(["admin", "artifacts", "byId", "9"])?.status).toBe("error");
  });

  // The typed text must still be there — not replaced by the error panel.
  expect(screen.getByLabelText(/content/i)).toHaveValue("IMPORTANT UNSAVED WORK");
  expect(screen.queryByText(/failed to load artifact/i)).not.toBeInTheDocument();
});
