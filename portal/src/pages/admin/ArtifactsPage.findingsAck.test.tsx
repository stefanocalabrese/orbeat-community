/**
 * Task 6 (docs/plans/orbeat-scan-acknowledgment-2026-08-27.md): the author's
 * own action on a pending submission the scanner flagged. POST
 * .../acknowledge-findings is submitter-only server-side (403 otherwise), so
 * the action is offered only on the artifact's own submitter's view of it,
 * and only while it is still pending with an unacknowledged CURRENT digest.
 *
 * Mocks fetch, like every other portal unit test in this repo: proves the
 * client-side state machine, not the real API<->portal seam.
 */
import { render, screen } from "@testing-library/react";
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

const base = {
  type: "skill" as const,
  description: "d",
  content: "body",
  memoryScope: null,
  memorySeed: null,
  version: "",
  visibility: "org" as const,
  approvalState: "pending" as const,
  approved: false,
  rowVersion: 1,
  minRevision: 0,
};

function renderPage(fetchImpl: typeof globalThis.fetch) {
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <ArtifactsPage />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
  return spy;
}

function serveArtifacts(artifacts: unknown[]) {
  return (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts, limit: 100, nextCursor: "" }));
  };
}

beforeEach(() => vi.restoreAllMocks());

test("no acknowledge action is offered for a clean pending submission (no findings)", async () => {
  renderPage(serveArtifacts([{ ...base, id: "1", name: "clean", submittedBy: "boss", findingsAcknowledged: false }]));
  expect(await screen.findByText("clean")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /acknowledge findings/i })).not.toBeInTheDocument();
  expect(screen.queryByText(/scanner flagged/i)).not.toBeInTheDocument();
});

test("shows the findings and an acknowledge action on the submitter's own unacknowledged pending artifact", async () => {
  renderPage(
    serveArtifacts([
      {
        ...base,
        id: "1",
        name: "risky",
        submittedBy: "boss",
        scanFindingsDigest: "digest-abc",
        findingsAcknowledged: false,
        scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" }],
      },
    ]),
  );
  expect(await screen.findByText("risky")).toBeInTheDocument();
  expect(screen.getByText(/content exceeds 64KiB/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /acknowledge findings/i })).toBeInTheDocument();
});

test("no acknowledge action for someone else's pending submission, even with unacknowledged findings", async () => {
  renderPage(
    serveArtifacts([
      {
        ...base,
        id: "1",
        name: "not-mine",
        submittedBy: "alice",
        scanFindingsDigest: "digest-abc",
        findingsAcknowledged: false,
        scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" }],
      },
    ]),
  );
  expect(await screen.findByText("not-mine")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /acknowledge findings/i })).not.toBeInTheDocument();
  expect(screen.queryByText(/content exceeds 64KiB/)).not.toBeInTheDocument();
});

test("no acknowledge action once the artifact is already acknowledged", async () => {
  renderPage(
    serveArtifacts([
      {
        ...base,
        id: "1",
        name: "already-ok",
        submittedBy: "boss",
        scanFindingsDigest: "digest-abc",
        findingsAcknowledged: true,
        findingsAckBy: "boss",
        findingsAckAt: "2026-08-27T00:00:00Z",
        scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" }],
      },
    ]),
  );
  expect(await screen.findByText("already-ok")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /acknowledge findings/i })).not.toBeInTheDocument();
});

test(
  "MANDATORY MUTANT: the acknowledge action sends the EXACT digest shown on the artifact, exactly once",
  async () => {
    const user = userEvent.setup();
    let ackCalls = 0;
    let ackBody: unknown;
    const fetchSpy = renderPage((input, init) => {
      const url = String(input instanceof Request ? input.url : input);
      if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
      if (init?.method === "POST" && url.includes("/acknowledge-findings")) {
        ackCalls += 1;
        ackBody = JSON.parse(String(init.body));
        return Promise.resolve(
          json({
            ...base,
            id: "1",
            name: "risky",
            submittedBy: "boss",
            scanFindingsDigest: "digest-xyz-987",
            findingsAcknowledged: true,
            findingsAckBy: "boss",
            findingsAckAt: "2026-08-27T00:00:00Z",
            scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" }],
          }),
        );
      }
      return Promise.resolve(
        json({
          artifacts: [
            {
              ...base,
              id: "1",
              name: "risky",
              submittedBy: "boss",
              scanFindingsDigest: "digest-xyz-987",
              findingsAcknowledged: false,
              scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" }],
            },
          ],
          limit: 100,
          nextCursor: "",
        }),
      );
    });

    await user.click(await screen.findByRole("button", { name: /acknowledge findings/i }));

    await vi.waitFor(() => expect(ackCalls).toBe(1));
    expect(ackBody).toEqual({ digest: "digest-xyz-987" });

    // Exactly one acknowledge POST fired for one click: no accidental
    // double submit, and never a hardcoded/empty digest standing in for the
    // artifact's real one.
    const ackPosts = fetchSpy.mock.calls.filter(([call, init]) => {
      const url = String(call instanceof Request ? call.url : call);
      return init?.method === "POST" && url.includes("acknowledge-findings");
    });
    expect(ackPosts).toHaveLength(1);
  },
);

test("after acknowledging, the state visibly changes: the prompt disappears", async () => {
  const user = userEvent.setup();
  let acked = false;
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "POST" && url.includes("/acknowledge-findings")) {
      acked = true;
      return Promise.resolve(
        json({
          ...base,
          id: "1",
          name: "risky",
          submittedBy: "boss",
          scanFindingsDigest: "digest-abc",
          findingsAcknowledged: true,
          findingsAckBy: "boss",
          findingsAckAt: "2026-08-27T00:00:00Z",
        }),
      );
    }
    return Promise.resolve(
      json({
        artifacts: [
          {
            ...base,
            id: "1",
            name: "risky",
            submittedBy: "boss",
            scanFindingsDigest: "digest-abc",
            findingsAcknowledged: acked,
            findingsAckBy: acked ? "boss" : undefined,
            findingsAckAt: acked ? "2026-08-27T00:00:00Z" : undefined,
            scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" }],
          },
        ],
        limit: 100,
        nextCursor: "",
      }),
    );
  });

  await user.click(await screen.findByRole("button", { name: /acknowledge findings/i }));
  await vi.waitFor(() =>
    expect(screen.queryByRole("button", { name: /acknowledge findings/i })).not.toBeInTheDocument(),
  );
});

test("a digest mismatch (412, a re-scan superseded it) shows the reload notice, and Reload refetches", async () => {
  const user = userEvent.setup();
  let ackCalls = 0;
  let getCalls = 0;
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "POST" && url.includes("/acknowledge-findings")) {
      ackCalls += 1;
      return Promise.resolve(
        json(
          {
            error: {
              message: "the submitted digest does not match the artifact's current findings; re-read the artifact and try again",
            },
          },
          412,
        ),
      );
    }
    getCalls += 1;
    return Promise.resolve(
      json({
        artifacts: [
          {
            ...base,
            id: "1",
            name: "risky",
            submittedBy: "boss",
            scanFindingsDigest: "digest-old",
            findingsAcknowledged: false,
            scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" }],
          },
        ],
        limit: 100,
        nextCursor: "",
      }),
    );
  });

  await user.click(await screen.findByRole("button", { name: /acknowledge findings/i }));

  expect(
    await screen.findByText(/this changed since you loaded it — reload to see the current state/i),
  ).toBeInTheDocument();
  expect(ackCalls).toBe(1);

  const getCallsAtConflict = getCalls;
  await user.click(screen.getByRole("button", { name: /reload/i }));
  await vi.waitFor(() => expect(getCalls).toBeGreaterThan(getCallsAtConflict));
});
