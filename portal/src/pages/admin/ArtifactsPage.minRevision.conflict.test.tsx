/**
 * Task 8 (spec §10): the 412 unhappy path for setting/clearing the artifact
 * minimum-revision floor.
 *
 * See ServersPage.conflict.test.tsx for the wording rationale, pinned by
 * TestNoOpUpdateStillBumpsRowVersion (internal/store/concurrency_test.go): a
 * 412 does not imply another admin was involved. Also pinned here, the
 * v1.23.0 Task 11 lesson: the Reload button must never resubmit the stale
 * PUT. Asserted by counting PUT calls, not by reading the button's type
 * attribute, because a defaulted type="submit" only misbehaves when the
 * button is inside a <form> RevisionHistory happens not to be.
 *
 * Mocks fetch; cannot prove the real seam (see C7 / Task 9).
 *
 * The "Require this or newer" button exercised below is now gated on
 * GET /v1/me's features.pinning (open-points.md point 6), so every fetch
 * mock in this file mocks that endpoint returning pinning: true (this
 * repo's own Enterprise build) as its FIRST branch, ahead of the
 * call-counting fallback the first test below relies on to hand back v5
 * then v6: an unmocked /v1/me would otherwise fall into that same
 * fallback and consume one of its two counted responses.
 */
import { render, screen, within } from "@testing-library/react";
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
const meOn = { subject: "boss", email: "b@x", roles: ["orbeat-admin", "orbeat-user"], features: { pinning: true } };

const revisions = [
  { revision: 2, source: "approval", content: "V2", approvedBy: "bob", approvedAt: "2026-07-12T10:00:00Z", isCurrent: true },
  { revision: 1, source: "approval", content: "V1", approvedBy: "bob", approvedAt: "2026-07-12T09:00:00Z", isCurrent: false },
];

const artifactV5 = {
  id: "h1", type: "skill" as const, name: "hist", description: "d", content: "c",
  memoryScope: null, memorySeed: null, version: "1.0.0",
  approvalState: "approved" as const, approved: true, visibility: "org" as const,
  rowVersion: 5, minRevision: 0,
};
const artifactV6 = { ...artifactV5, rowVersion: 6 };

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

test("412 on set-floor: shows the reload message, reload refetches the artifact list, and the PUT is never auto-retried", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  let listCalls = 0;
  let putCalls = 0;
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    if (method === "PUT" && url.endsWith("/h1/min-revision")) {
      putCalls += 1;
      return Promise.resolve(json({ error: { message: "row_version mismatch" } }, 412));
    }
    listCalls += 1;
    return Promise.resolve(json({ artifacts: [listCalls === 1 ? artifactV5 : artifactV6], limit: 100, nextCursor: "" }));
  });

  const tr = (await screen.findByText("hist")).closest("tr") as HTMLElement;
  await user.click(within(tr).getByRole("button", { name: /^history for/i }));
  const row1 = (await screen.findByText("#1")).closest("li") as HTMLElement;
  await user.click(within(row1).getByRole("button", { name: /require this or newer/i }));

  expect(
    await screen.findByText(/this changed since you loaded it.+reload to see the current state/i),
  ).toBeInTheDocument();
  expect(screen.queryByText(/someone else/i)).not.toBeInTheDocument();
  expect(putCalls).toBe(1);

  const listCallsAtConflict = listCalls;
  await user.click(screen.getByRole("button", { name: /reload/i }));
  await vi.waitFor(() => expect(listCalls).toBeGreaterThan(listCallsAtConflict));

  // Never auto-retried the write.
  expect(putCalls).toBe(1);
});

test("a non-412 set-floor error still renders the ordinary error path, the conflict branch must not swallow it", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    if (method === "PUT" && url.endsWith("/h1/min-revision")) return Promise.resolve(json({ error: { message: "boom" } }, 500));
    return Promise.resolve(json({ artifacts: [artifactV5], limit: 100, nextCursor: "" }));
  });

  const tr = (await screen.findByText("hist")).closest("tr") as HTMLElement;
  await user.click(within(tr).getByRole("button", { name: /^history for/i }));
  const row1 = (await screen.findByText("#1")).closest("li") as HTMLElement;
  await user.click(within(row1).getByRole("button", { name: /require this or newer/i }));

  expect(await screen.findByText(/boom/)).toBeInTheDocument();
  expect(screen.queryByText(/reload to see the current state/i)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /reload/i })).not.toBeInTheDocument();
});
