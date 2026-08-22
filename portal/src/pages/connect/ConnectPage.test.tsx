import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ConnectPage from "./ConnectPage";

const alice = { isLoading: false, authenticated: true, roles: ["orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "alice", email: "a@x" };

beforeEach(() => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ servers: [
      { id: "1", name: "github", description: "", transport: "http", version: "", protocolVersion: "", status: "active", allowedTools: ["create_issue"] },
    ] }), { status: 200, headers: { "Content-Type": "application/json" } }),
  );
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <AuthCtx.Provider value={alice}>
      <QueryClientProvider client={qc}><MemoryRouter><ConnectPage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

test("shows native plugin install commands and team snippet", () => {
  renderPage();
  expect(screen.getByText("claude plugin marketplace add ./marketplace")).toBeInTheDocument();
  expect(screen.getByText("claude plugin install orbeat-gateway@orbeat")).toBeInTheDocument();
  // team settings.json snippet enables the plugin
  expect(screen.getByText(/"orbeat-gateway@orbeat": true/)).toBeInTheDocument();
});

test("shows artifacts plugin install command", () => {
  renderPage();
  expect(screen.getByText("claude plugin install orbeat-artifacts@orbeat")).toBeInTheDocument();
});

test("a failing catalog query renders the error panel, not the 'Nothing yet' empty state", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(
      new Response(JSON.stringify({ error: { message: "boom" } }), {
        status: 500,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
  renderPage();
  expect(await screen.findByText(/failed to load entitled servers/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  expect(screen.queryByText(/nothing yet — ask an admin/i)).not.toBeInTheDocument();
});

test("keeps manual fallback: gateway endpoint, claude mcp add, entitled tools", async () => {
  renderPage();
  expect(screen.getAllByText(/http:\/\/localhost:8090\/mcp/).length).toBeGreaterThan(0);
  expect(screen.getByText(/claude mcp add/)).toBeInTheDocument();
  expect(await screen.findByText("github")).toBeInTheDocument();
  expect(screen.getByText(/create_issue/)).toBeInTheDocument();
});

test("a Copy button flips to 'Copied' when the clipboard write resolves", async () => {
  const user = userEvent.setup();
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  renderPage();
  await user.click(screen.getByRole("button", { name: "Copy Gateway endpoint" }));
  expect(await screen.findByText("Copied")).toBeInTheDocument();
  expect(writeText).toHaveBeenCalledWith("http://localhost:8090/mcp");
});

test("a Copy button shows a failure state when the clipboard write rejects (non-secure origin)", async () => {
  const user = userEvent.setup();
  const writeText = vi.fn().mockRejectedValue(new Error("NotAllowedError"));
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  renderPage();
  await user.click(screen.getByRole("button", { name: "Copy Gateway endpoint" }));
  expect(await screen.findByText(/copy failed/i)).toBeInTheDocument();
});
