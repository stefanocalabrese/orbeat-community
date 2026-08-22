/**
 * Task 11 / C8 #1: ReviewQueuePage's per-card `Diff` reads content,
 * memorySeed, approvedContent and approvedMemorySeed off
 * GET /v1/admin/artifacts?state=pending — all four are omitted from the
 * default slim projection Task 8 shipped. Without `?include=content` the
 * approver's diff panel renders blank: they approve without seeing what
 * they approve, on the exact governance surface Phase 4 exists to provide.
 *
 * This test mocks `fetch`, like every other portal unit test in this repo —
 * it can only prove the CLIENT sends `include=content`, never that the real
 * API honors it end to end over the wire. That seam is Task 12's Playwright
 * e2e spec against the real compose stack, not this file (see C7).
 */
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi, beforeEach } from "vitest";
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

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
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

describe("ReviewQueuePage", () => {
  it("requests the review queue WITH content — the diff renders it", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ artifacts: [], limit: 50, nextCursor: "" }),
    });
    vi.stubGlobal("fetch", fetchMock);

    renderPage();
    // Wait for the query to resolve (empty-queue hint) so the fetch call
    // below is guaranteed to have already happened.
    await screen.findByText(/nothing pending review/i);

    const url = String(fetchMock.mock.calls[0]?.[0] ?? "");
    expect(url).toContain("state=pending");
    expect(url).toContain("include=content");
  });
});
