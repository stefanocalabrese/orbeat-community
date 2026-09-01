/**
 * B35: `historyArtifact` is derived by looking up `historyFor` inside
 * `artifacts` (ArtifactsPage.tsx), which comes straight off
 * useAdminArtifacts's query. Changing the sort or search box changes that
 * query's key (order/q are folded into it — queries.ts's useAdminList), and
 * a key change means `query.data` (and therefore `artifacts`) goes back to
 * whatever the NEW key's cache holds while its own request is in flight —
 * empty on a first visit to that key. `historyArtifact` then resolves to
 * `undefined` and the Version history panel unmounts with no warning,
 * mid-request, even though the artifact it was showing is still on its way
 * back in the very same response.
 *
 * Mocks fetch, like every other portal unit test in this repo — see the
 * report for what this can't prove about a real, slower network.
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ArtifactsPage from "./ArtifactsPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin", "orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const publishStatus = { lastAttemptAt: null, lastSuccessAt: null, lastCommit: "abc123", lastError: "" };
const artifactA = {
  id: "1", type: "skill" as const, name: "alpha", description: "", content: "body",
  memoryScope: null, memorySeed: null, version: "0.1.0", approvalState: "approved" as const,
  approved: true, visibility: "org" as const,
};

function renderPage(fetchImpl: typeof globalThis.fetch) {
  vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ArtifactsPage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("Version history stays mounted while a sort change's refetch is in flight, for an artifact that is still present in the new result", async () => {
  const user = userEvent.setup();
  let listCalls = 0;
  // Definite-assignment assertion: assigned inside the fetch mock below
  // (which fires before the later `resolveSecond(...)` call), but TS's
  // control-flow narrowing can't see across that closure boundary.
  let resolveSecond!: (r: Response) => void;
  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions: [], limit: 100, nextCursor: "" }));
    if (url.includes("order=desc")) {
      listCalls += 1;
      // The SECOND (sorted) request for the artifact list hangs until the
      // test explicitly resolves it, so the "in flight" window is
      // observable rather than racing a same-tick microtask.
      return new Promise<Response>((resolve) => {
        resolveSecond = resolve;
      });
    }
    return Promise.resolve(json({ artifacts: [artifactA], limit: 100, nextCursor: "" }));
  });

  await screen.findByText("alpha");
  await user.click(screen.getByRole("button", { name: /history for alpha/i }));
  expect(await screen.findByText("Version history")).toBeInTheDocument();

  // Trigger the sort change — the SAME artifact is still in the eventual
  // result, only reordered.
  await user.click(screen.getByRole("button", { name: "Type" }));
  await vi.waitFor(() => expect(listCalls).toBe(1));

  // While that request is still pending, the panel must still be there.
  expect(screen.getByText("Version history")).toBeInTheDocument();

  resolveSecond(json({ artifacts: [artifactA], limit: 100, nextCursor: "" }));
  await vi.waitFor(() => expect(screen.getByText("Version history")).toBeInTheDocument());
});
