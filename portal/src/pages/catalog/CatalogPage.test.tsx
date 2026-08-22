import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import CatalogPage from "./CatalogPage";

const alice = { isLoading: false, authenticated: true, roles: ["orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "alice", email: "a@x" };

beforeEach(() => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ servers: [
      { id: "1", name: "github", description: "GH tools", transport: "http", version: "", protocolVersion: "", status: "active", allowedTools: ["create_issue"] },
      { id: "2", name: "docs", description: "", transport: "sse", version: "", protocolVersion: "", status: "active", allowedTools: null },
    ] }), { status: 200, headers: { "Content-Type": "application/json" } }),
  );
});

test("renders entitled servers as cards with Connect links", async () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <AuthCtx.Provider value={alice}>
      <QueryClientProvider client={qc}>
        <MemoryRouter><CatalogPage /></MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
  expect(await screen.findByText("github")).toBeInTheDocument();
  expect(screen.getByText("docs")).toBeInTheDocument();
  expect(screen.getAllByRole("link", { name: /connect/i }).length).toBeGreaterThan(0);
});
