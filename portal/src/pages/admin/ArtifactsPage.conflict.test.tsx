/**
 * Task 11 (spec §10): the 412 unhappy path for an artifact edit save.
 *
 * See ServersPage.conflict.test.tsx for the wording rationale — pinned by
 * TestNoOpUpdateStillBumpsRowVersion (internal/store/concurrency_test.go):
 * a 412 does not imply another admin was involved, so the copy here never
 * says so. Also pinned here: no automatic retry of the PUT.
 *
 * Mocks fetch; cannot prove the real seam (see C7 / Task 12).
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

const artifactV3 = {
  id: "9",
  type: "skill" as const,
  name: "fmt",
  description: "d",
  content: "body",
  memoryScope: null,
  memorySeed: null,
  version: "0.1.0",
  approvalState: "approved" as const,
  approved: true,
  visibility: "org" as const,
  rowVersion: 3,
};
const artifactV4 = { ...artifactV3, rowVersion: 4 };

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

test("412 on save: shows the reload message, reload refetches the by-id query, and the PUT is never auto-retried", async () => {
  const user = userEvent.setup();
  let byIdCalls = 0;
  let putCalls = 0;
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (method === "PUT" && url.endsWith("/artifacts/9")) {
      putCalls += 1;
      return Promise.resolve(json({ error: { message: "row_version mismatch" } }, 412));
    }
    if (method === "GET" && url.endsWith("/artifacts/9")) {
      byIdCalls += 1;
      return Promise.resolve(json(byIdCalls === 1 ? artifactV3 : artifactV4));
    }
    return Promise.resolve(json({ artifacts: [artifactV3], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit fmt$/i }));
  await screen.findByLabelText(/content/i);
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(
    await screen.findByText(/this changed since you loaded it — reload to see the current state/i),
  ).toBeInTheDocument();
  expect(screen.queryByText(/someone else/i)).not.toBeInTheDocument();
  expect(putCalls).toBe(1);

  const byIdCallsAtConflict = byIdCalls;
  await user.click(screen.getByRole("button", { name: /reload/i }));
  await vi.waitFor(() => expect(byIdCalls).toBeGreaterThan(byIdCallsAtConflict));

  // Never auto-retried the write.
  expect(putCalls).toBe(1);
  // Reload discards the in-progress edit: the form is gone, the admin must
  // reopen Edit to see the fresh artifact and reapply their change.
  expect(screen.queryByRole("button", { name: /^save$/i })).not.toBeInTheDocument();
});

test("a non-412 save error still renders the ordinary error path — the conflict branch must not swallow it", async () => {
  const user = userEvent.setup();
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (method === "PUT" && url.endsWith("/artifacts/9")) return Promise.resolve(json({ error: { message: "boom" } }, 500));
    if (method === "GET" && url.endsWith("/artifacts/9")) return Promise.resolve(json(artifactV3));
    return Promise.resolve(json({ artifacts: [artifactV3], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit fmt$/i }));
  await screen.findByLabelText(/content/i);
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(await screen.findByText(/boom/)).toBeInTheDocument();
  expect(screen.queryByText(/reload to see the current state/i)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /reload/i })).not.toBeInTheDocument();
});
