/**
 * Task 11 (spec §10): the 412 unhappy path for a server edit save.
 *
 * The wording is pinned deliberately, not left to guesswork: the row_version
 * trigger bumps on EVERY update, including a no-op — see
 * TestNoOpUpdateStillBumpsRowVersion (internal/store/concurrency_test.go).
 * A client retry after a dropped response (e.g. a network timeout) therefore
 * produces a 412 with nobody else involved: the first attempt already
 * succeeded, the client just never heard about it. "someone else edited
 * this" would be false in that (not rare) case, so the message never claims
 * it — it says the row changed, not who changed it.
 *
 * Also pinned: no automatic retry. A silent retry would resubmit the exact
 * same (now stale) body against the fresh row_version the reload just
 * fetched — i.e. last-write-wins wearing a hat, which is what this whole
 * slice exists to remove. Reload only refetches; it never re-calls the PUT
 * itself.
 *
 * Mocks fetch, like every other portal unit test in this repo. This proves
 * the component logic only — it cannot prove the real API actually returns
 * 412 the way these mocks assume, or that a real cross-origin PUT carries
 * If-Match correctly. That seam is Task 12's Playwright e2e (spec §10.1
 * requires a server edit+save e2e specifically, not just an artifact one).
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ServersPage from "./ServersPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin", "orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const serverV3 = { id: "1", name: "github", description: "", transport: "http", endpointOrCommand: "https://x", version: "", protocolVersion: "", status: "active", hasSecret: false, rowVersion: 3 };
const serverV4 = { ...serverV3, rowVersion: 4 };

function renderPage(fetchImpl: typeof globalThis.fetch) {
  vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ServersPage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("412 on save: shows the reload message (not a 'someone else' claim), reload refetches, and the PUT is never auto-retried", async () => {
  const user = userEvent.setup();
  let getCalls = 0;
  let putCalls = 0;
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      putCalls += 1;
      return Promise.resolve(json({ error: { message: "row_version mismatch" } }, 412));
    }
    getCalls += 1;
    return Promise.resolve(json({ servers: [getCalls === 1 ? serverV3 : serverV4], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit /i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(
    await screen.findByText(/this changed since you loaded it — reload to see the current state/i),
  ).toBeInTheDocument();
  expect(screen.queryByText(/someone else/i)).not.toBeInTheDocument();
  expect(putCalls).toBe(1);

  const getCallsAtConflict = getCalls;
  await user.click(screen.getByRole("button", { name: /reload/i }));
  await vi.waitFor(() => expect(getCalls).toBeGreaterThan(getCallsAtConflict));

  // Never auto-retried the write.
  expect(putCalls).toBe(1);
  // Reload discards the in-progress edit: the save form is gone, the admin
  // must reopen Edit to see the fresh row and reapply their change.
  expect(screen.queryByRole("button", { name: /^save$/i })).not.toBeInTheDocument();
});

test("a non-412 save error still renders the ordinary error path — the conflict branch must not swallow it", async () => {
  const user = userEvent.setup();
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      return Promise.resolve(json({ error: { message: "boom" } }, 500));
    }
    return Promise.resolve(json({ servers: [serverV3], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit /i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(await screen.findByText(/boom/)).toBeInTheDocument();
  expect(screen.queryByText(/reload to see the current state/i)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /reload/i })).not.toBeInTheDocument();
});
