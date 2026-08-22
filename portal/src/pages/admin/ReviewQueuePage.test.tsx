import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import type { AdminArtifact } from "../../api/types";
import ReviewQueuePage from "./ReviewQueuePage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin", "orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function renderReviewQueue(artifacts: AdminArtifact[]) {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/admin/artifacts") && url.includes("state=pending")) {
      return Promise.resolve(json({ artifacts, limit: 50, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 50, nextCursor: "" }));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ReviewQueuePage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("lists a pending artifact with findings and an Approve button", async () => {
  renderReviewQueue([
    {
      id: "p1", type: "skill", name: "risky", description: "d",
      content: "proposed body", memoryScope: null, memorySeed: null, version: "",
      visibility: "org", approvalState: "pending", approved: false,
      submittedBy: "alice", rowVersion: 1,
      scanFindings: [{ rule: "size", message: "content exceeds 64KiB", severity: "warn" }],
    },
  ]);
  expect(await screen.findByText("risky")).toBeInTheDocument();
  expect(screen.getByText(/content exceeds 64KiB/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /approve/i })).toBeInTheDocument();
});

test("a pending rule artifact renders a styled type chip (no undefined class)", async () => {
  renderReviewQueue([
    {
      id: "r1", type: "rule", name: "org-std", description: "d",
      content: "Use tabs.", memoryScope: null, memorySeed: null, version: "",
      visibility: "role", approvalState: "pending", approved: false,
      submittedBy: "alice", rowVersion: 1,
    },
  ]);
  expect(await screen.findByText("org-std")).toBeInTheDocument();
  const chip = screen.getByText("rule");
  expect(chip.className).not.toMatch(/undefined/);
  // a real token style, not just the base chip classes
  expect(chip.className).toMatch(/bg-/);
});

test("a failing queue query renders the error panel, not the empty-queue hint", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(json({ error: { message: "boom" } }, 500)),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ReviewQueuePage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
  expect(await screen.findByText(/failed to load the review queue/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  expect(screen.queryByText(/nothing pending review/i)).not.toBeInTheDocument();
});

test("empty queue shows a hint and no cards", async () => {
  renderReviewQueue([]);
  expect(await screen.findByText(/nothing pending review/i)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /approve/i })).not.toBeInTheDocument();
});

test("shows the currently-live content in the diff pane alongside the proposed content", async () => {
  renderReviewQueue([
    {
      id: "p2", type: "skill", name: "rev", description: "d",
      content: "new proposed body", memoryScope: null, memorySeed: null, version: "",
      visibility: "org", approvalState: "pending", approved: false,
      submittedBy: "bob", approvedContent: "old live body", rowVersion: 1,
    },
  ]);
  expect(await screen.findByText("new proposed body")).toBeInTheDocument();
  expect(screen.getByText("old live body")).toBeInTheDocument();
});

test("reject sends the typed reason", async () => {
  let rejectBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (init?.method === "POST" && url.includes("/reject")) {
      rejectBody = JSON.parse(String(init.body));
      return Promise.resolve(json({ artifacts: [], limit: 50, nextCursor: "" }));
    }
    if (url.includes("/admin/artifacts") && url.includes("state=pending")) {
      return Promise.resolve(
        json({
          artifacts: [
            {
              id: "p3", type: "skill", name: "sus", description: "d",
              content: "body", memoryScope: null, memorySeed: null, version: "",
              visibility: "org", approvalState: "pending", approved: false,
              submittedBy: "carol", rowVersion: 1,
            },
          ],
          limit: 50,
          nextCursor: "",
        }),
      );
    }
    return Promise.resolve(json({ artifacts: [], limit: 50, nextCursor: "" }));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const { default: userEvent } = await import("@testing-library/user-event");
  const user = userEvent.setup();
  render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ReviewQueuePage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
  await user.type(await screen.findByLabelText(/reject reason/i), "bad content");
  await user.click(screen.getByRole("button", { name: /^reject$/i }));
  await vi.waitFor(() => expect(rejectBody).toBeDefined());
  expect((rejectBody as { reason: string }).reason).toBe("bad content");
});

// The Reject button deliberately stays enabled with an empty reason box —
// there is no client-side required-field guard, so exactly one rule exists
// and it lives on the server (fable-audit §7 #17). This pins the rest of the
// chain: the server's 400 `{"error":{"message":"rejection reason is
// required"}}` is parsed into an `ApiRequestError`, `errMsg` returns its
// message verbatim, and ReviewQueuePage renders it inline under the buttons.
// Both this assertion and portal/e2e's reject spec key on that exact string —
// if the server's wording ever changes, both must change together.
test("reject with a blank reason surfaces the server's required-reason message inline", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (init?.method === "POST" && url.includes("/reject")) {
      return Promise.resolve(
        json({ error: { message: "rejection reason is required" } }, 400),
      );
    }
    if (url.includes("/admin/artifacts") && url.includes("state=pending")) {
      return Promise.resolve(
        json({
          artifacts: [
            {
              id: "p4", type: "skill", name: "unreasoned", description: "d",
              content: "body", memoryScope: null, memorySeed: null, version: "",
              visibility: "org", approvalState: "pending", approved: false,
              submittedBy: "dave", rowVersion: 1,
            },
          ],
          limit: 50,
          nextCursor: "",
        }),
      );
    }
    return Promise.resolve(json({ artifacts: [], limit: 50, nextCursor: "" }));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const { default: userEvent } = await import("@testing-library/user-event");
  const user = userEvent.setup();
  render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ReviewQueuePage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
  await screen.findByText("unreasoned");
  await user.click(screen.getByRole("button", { name: /^reject$/i }));
  expect(await screen.findByText("rejection reason is required")).toBeInTheDocument();
});
