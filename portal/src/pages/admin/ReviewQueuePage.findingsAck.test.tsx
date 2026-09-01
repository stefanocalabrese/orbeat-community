/**
 * Task 7 (docs/plans/orbeat-scan-acknowledgment-2026-08-27.md): the
 * approver's own acknowledgment gate on Approve.
 *
 * Two refusals share one 409 status, distinguished by a machine-readable
 * `code` (internal/api/precondition.go): the AUTHOR has not acknowledged the
 * artifact's current findings, or THIS request does not carry the
 * APPROVER's own acknowledgment of it. The portal's job is to never let the
 * second one happen (the approver ticks a checkbox that sends the digest) and
 * to never even attempt the request while the first is true (the artifact's
 * own `findingsAcknowledged` field already says so, server-computed --
 * never recomputed here).
 *
 * Mocks fetch, like every other portal unit test in this repo.
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import type { AdminArtifact } from "../../api/types";
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
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const base = {
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
  rowVersion: 3,
  minRevision: 0,
  scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" as const }],
};

type FetchExtra = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response> | null;

function renderQueue(artifact: Partial<AdminArtifact> & { id: string }, fetchExtra?: FetchExtra) {
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (fetchExtra) {
      const extra = fetchExtra(input, init);
      if (extra) return extra;
    }
    if (url.includes("/admin/artifacts") && url.includes("state=pending")) {
      return Promise.resolve(json({ artifacts: [artifact], limit: 50, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 50, nextCursor: "" }));
  });
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
  return spy;
}

beforeEach(() => vi.restoreAllMocks());

test("Approve is disabled and explains itself when the author has not acknowledged findings", async () => {
  renderQueue({ ...base, scanFindingsDigest: "d1", findingsAcknowledged: false });

  await screen.findByText("risky");
  expect(screen.getByText(/waiting on alice to acknowledge/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^approve$/i })).toBeDisabled();
  // Nothing for the approver to tick yet -- ticking would be meaningless
  // while the author has not acknowledged.
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

test("Approve is blocked until the approver ticks, and no request fires from clicking a disabled button", async () => {
  const user = userEvent.setup();
  let approveCalls = 0;
  const fetchSpy = renderQueue(
    { ...base, scanFindingsDigest: "d1", findingsAcknowledged: true, findingsAckBy: "alice" },
    (input, init) => {
      const url = String(input instanceof Request ? input.url : input);
      if (init?.method === "POST" && url.includes("/approve")) {
        approveCalls += 1;
        return Promise.resolve(json({ ...base, approvalState: "approved", approved: true }));
      }
      return null;
    },
  );

  await screen.findByText("risky");
  const approveBtn = screen.getByRole("button", { name: /^approve$/i });
  const checkbox = screen.getByRole("checkbox", { name: /reviewed the findings for risky/i });
  expect(checkbox).not.toBeChecked();
  expect(approveBtn).toBeDisabled();

  // MANDATORY MUTANT: a click on a disabled Approve button must never issue
  // a request -- if a future change drops the disabled gate, this goes red.
  await user.click(approveBtn);
  expect(approveCalls).toBe(0);

  await user.click(checkbox);
  expect(approveBtn).not.toBeDisabled();
  await user.click(approveBtn);

  await vi.waitFor(() => expect(approveCalls).toBe(1));
  const approvePosts = fetchSpy.mock.calls.filter(([call, init]) => {
    const url = String(call instanceof Request ? call.url : call);
    return init?.method === "POST" && url.includes("/approve");
  });
  expect(approvePosts).toHaveLength(1);
  expect(JSON.parse(String(approvePosts[0]?.[1]?.body))).toEqual({ acknowledgedFindingsDigest: "d1" });
});

test("a clean artifact (no findings) approves exactly as before: no checkbox, no body field, enabled immediately", async () => {
  const user = userEvent.setup();
  let approveBody: unknown;
  let approveInit: RequestInit | undefined;
  renderQueue({ ...base, scanFindings: undefined, findingsAcknowledged: false }, (input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (init?.method === "POST" && url.includes("/approve")) {
      approveInit = init;
      approveBody = init.body;
      return Promise.resolve(json({ ...base, approvalState: "approved", approved: true }));
    }
    return null;
  });

  await screen.findByText("risky");
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  const approveBtn = screen.getByRole("button", { name: /^approve$/i });
  expect(approveBtn).not.toBeDisabled();
  await user.click(approveBtn);

  await vi.waitFor(() => expect(approveInit).toBeDefined());
  expect(approveBody).toBeUndefined();
});

test("a findings-ack-required 409 that slips through (a stale queue row) shows the reload notice, not a raw error", async () => {
  const user = userEvent.setup();
  renderQueue(
    { ...base, scanFindingsDigest: "d1", findingsAcknowledged: true, findingsAckBy: "alice" },
    (input, init) => {
      const url = String(input instanceof Request ? input.url : input);
      if (init?.method === "POST" && url.includes("/approve")) {
        return Promise.resolve(
          json(
            {
              error: { message: "acknowledge the current scan findings before approving" },
              code: "approver_findings_ack_required",
            },
            409,
          ),
        );
      }
      return null;
    },
  );

  await screen.findByText("risky");
  await user.click(screen.getByRole("checkbox", { name: /reviewed the findings for risky/i }));
  await user.click(screen.getByRole("button", { name: /^approve$/i }));

  expect(
    await screen.findByText(/this changed since you loaded it — reload to see the current state/i),
  ).toBeInTheDocument();
});
