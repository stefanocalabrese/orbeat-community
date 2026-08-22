/**
 * Task 11 (spec §10): the 412 unhappy path for approve.
 *
 * See ServersPage.conflict.test.tsx for the wording rationale — pinned by
 * TestNoOpUpdateStillBumpsRowVersion (internal/store/concurrency_test.go):
 * a 412 does not imply another admin was involved. Also pinned here: no
 * automatic retry of the approve POST.
 *
 * Mocks fetch; cannot prove the real seam (see C7 / Task 12).
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ReviewQueuePage from "./ReviewQueuePage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin", "orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const pendingV1 = {
  id: "p1",
  type: "skill" as const,
  name: "risky",
  description: "d",
  content: "proposed body",
  memoryScope: null,
  memorySeed: null,
  version: "",
  visibility: "org" as const,
  approvalState: "pending" as const,
  approved: false,
  submittedBy: "alice",
  rowVersion: 1,
};
const pendingV2 = { ...pendingV1, rowVersion: 2 };

function renderPage(fetchImpl: typeof globalThis.fetch) {
  vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ReviewQueuePage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("412 on approve: shows the reload message, reload refetches the queue, and approve is never auto-retried", async () => {
  const user = userEvent.setup();
  let listCalls = 0;
  let approveCalls = 0;
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (method === "POST" && url.includes("/approve")) {
      approveCalls += 1;
      return Promise.resolve(json({ error: { message: "row_version mismatch" } }, 412));
    }
    if (url.includes("/admin/artifacts") && url.includes("state=pending")) {
      listCalls += 1;
      return Promise.resolve(json({ artifacts: [listCalls === 1 ? pendingV1 : pendingV2], limit: 50, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 50, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /approve/i }));

  expect(
    await screen.findByText(/this changed since you loaded it — reload to see the current state/i),
  ).toBeInTheDocument();
  expect(screen.queryByText(/someone else/i)).not.toBeInTheDocument();
  expect(approveCalls).toBe(1);

  const listCallsAtConflict = listCalls;
  await user.click(screen.getByRole("button", { name: /reload/i }));
  await vi.waitFor(() => expect(listCalls).toBeGreaterThan(listCallsAtConflict));

  // Never auto-retried the write.
  expect(approveCalls).toBe(1);
});

test("a non-412 approve error still renders the ordinary error path — the conflict branch must not swallow it", async () => {
  const user = userEvent.setup();
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (method === "POST" && url.includes("/approve")) {
      return Promise.resolve(json({ error: { message: "boom" } }, 500));
    }
    if (url.includes("/admin/artifacts") && url.includes("state=pending")) {
      return Promise.resolve(json({ artifacts: [pendingV1], limit: 50, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 50, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /approve/i }));

  expect(await screen.findByText(/boom/)).toBeInTheDocument();
  expect(screen.queryByText(/reload to see the current state/i)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /reload/i })).not.toBeInTheDocument();
});
