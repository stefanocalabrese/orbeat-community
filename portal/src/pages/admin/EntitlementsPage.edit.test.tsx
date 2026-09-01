import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import EntitlementsPage from "./EntitlementsPage";
import { ToastProvider } from "../../components/ui/Toast";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (b: unknown, s = 200) => new Response(JSON.stringify(b), { status: s, headers: { "Content-Type": "application/json" } });

let requests: { url: string; method: string; ifMatch: string | null; body: unknown }[] = [];
let putStatus = 200;

beforeEach(() => {
  requests = [];
  putStatus = 200;
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    const headers = new Headers(init?.headers);
    requests.push({
      url, method,
      ifMatch: headers.get("If-Match"),
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
    });
    if (method === "PUT") {
      return Promise.resolve(putStatus === 200
        ? json({ id: "e1", roleId: "r1", mcpServerId: "s1", allowedTools: ["echo"], permissions: [], rowVersion: 8 })
        : json({ error: { message: "version mismatch" } }, putStatus));
    }
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user" }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/servers")) return Promise.resolve(json({ servers: [{ id: "s1", name: "github", status: "active", transport: "http", endpointOrCommand: "https://x", description: "", version: "", protocolVersion: "", rowVersion: 1 }], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({
      entitlements: [{ id: "e1", roleId: "r1", mcpServerId: "s1", allowedTools: ["echo", "search"], permissions: ["read"], rowVersion: 7 }],
      limit: 100, nextCursor: "",
    }));
  });
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><EntitlementsPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
}

test("editing a grant's tools sends a PUT with the row's If-Match and preserves permissions", async () => {
  const user = userEvent.setup();
  renderPage();
  expect(await screen.findByText("github")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /edit tools/i }));
  const field = screen.getByLabelText(/allowed tools for orbeat-user on github/i);
  expect(field).toHaveValue("echo, search");

  await user.clear(field);
  await user.type(field, "echo");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(requests.some((r) => r.method === "PUT")).toBe(true));
  const put = requests.find((r) => r.method === "PUT")!;
  expect(put.url).toContain("/v1/admin/entitlements/e1");
  // The row's own version, quoted: an unquoted If-Match is a 400, and a missing
  // one is a 428, so this assertion is the difference between an edit that
  // works and one that is refused.
  expect(put.ifMatch).toBe('"7"');
  expect(put.body).toEqual({ allowedTools: ["echo"], permissions: ["read"] });
});

test("clearing the field means ALL tools, never none", async () => {
  const user = userEvent.setup();
  renderPage();
  await screen.findByText("github");
  await user.click(screen.getByRole("button", { name: /edit tools/i }));
  await user.clear(screen.getByLabelText(/allowed tools/i));
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(requests.some((r) => r.method === "PUT")).toBe(true));
  const put = requests.find((r) => r.method === "PUT")!;
  // null, not []. An empty array denies every tool, so a half-finished edit
  // would silently revoke the grant instead of widening it.
  expect((put.body as { allowedTools: unknown }).allowedTools).toBeNull();
});

test("Cancel discards the draft without sending anything", async () => {
  const user = userEvent.setup();
  renderPage();
  await screen.findByText("github");
  await user.click(screen.getByRole("button", { name: /edit tools/i }));
  await user.clear(screen.getByLabelText(/allowed tools/i));
  await user.type(screen.getByLabelText(/allowed tools/i), "wrong");
  // Cancel sits inside a form via the `form` attribute, so an untyped button
  // would submit the draft it is meant to discard.
  await user.click(screen.getByRole("button", { name: /^cancel$/i }));

  await waitFor(() => expect(screen.queryByLabelText(/allowed tools/i)).not.toBeInTheDocument());
  expect(requests.some((r) => r.method === "PUT")).toBe(false);
});

test("a 412 tells the user someone else changed it, not a raw error", async () => {
  const user = userEvent.setup();
  putStatus = 412;
  renderPage();
  await screen.findByText("github");
  await user.click(screen.getByRole("button", { name: /edit tools/i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(await screen.findByText(/someone else changed this entitlement/i)).toBeInTheDocument();
});

// B35: EntitlementsPage never called update.reset(), so a stale 412 message
// outlived the edit it belonged to — visible even after Cancel discarded the
// draft, and still showing next to a BRAND NEW edit the admin had not yet
// attempted to save.

test("Cancel clears a stale 412 error, not just the draft", async () => {
  const user = userEvent.setup();
  putStatus = 412;
  renderPage();
  await screen.findByText("github");
  await user.click(screen.getByRole("button", { name: /edit tools/i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  expect(await screen.findByText(/someone else changed this entitlement/i)).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^cancel$/i }));

  expect(screen.queryByText(/someone else changed this entitlement/i)).not.toBeInTheDocument();
});

test("starting a NEW edit clears a stale 412 error left over from a prior attempt", async () => {
  const user = userEvent.setup();
  putStatus = 412;
  renderPage();
  await screen.findByText("github");
  await user.click(screen.getByRole("button", { name: /edit tools/i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  expect(await screen.findByText(/someone else changed this entitlement/i)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /^cancel$/i }));

  // Re-opening Edit tools (a fresh attempt) must not still be showing the
  // PREVIOUS attempt's failure before this one has even been tried.
  await user.click(screen.getByRole("button", { name: /edit tools/i }));
  expect(screen.queryByText(/someone else changed this entitlement/i)).not.toBeInTheDocument();
});

// The WIRING gate. Every test above renders the page without a ToastProvider,
// where useToast is a deliberate no-op, so removing the push from
// useInvalidating leaves them all green: a mutant proved exactly that. This one
// wraps the real provider around the real page and asserts the sentence a user
// would read, which is the only thing that can tell "the toast is wired" from
// "the toast exists".
test("a successful save tells the user, through the real provider", async () => {
  const user = userEvent.setup();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <ToastProvider>
          <MemoryRouter>
            <EntitlementsPage />
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
  await screen.findByText("github");
  await user.click(screen.getByRole("button", { name: /edit tools/i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(await screen.findByRole("status")).toHaveTextContent("Allowed tools updated.");
});
