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

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ServersPage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("lists servers with hasSecret badge", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ servers: [
    { id: "1", name: "github", description: "", transport: "http", endpointOrCommand: "https://x", version: "", protocolVersion: "", status: "active", hasSecret: true },
  ], limit: 100, nextCursor: "" }));
  renderPage();
  expect(await screen.findByText("github")).toBeInTheDocument();
  expect(screen.getByText(/secret/i)).toBeInTheDocument();
});

test("edit: switching from server A to server B shows B's values, not A's (unkeyed-form regression)", async () => {
  const user = userEvent.setup();
  const serverA = { id: "1", name: "github", description: "", transport: "http", endpointOrCommand: "https://a", version: "", protocolVersion: "", status: "active", hasSecret: false };
  const serverB = { id: "2", name: "gitlab", description: "", transport: "http", endpointOrCommand: "https://b", version: "", protocolVersion: "", status: "active", hasSecret: false };
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ servers: [serverA, serverB], limit: 100, nextCursor: "" }));
  renderPage();
  await screen.findByText("github");

  const [editA, editB] = screen.getAllByRole("button", { name: /^edit /i });
  if (!editA || !editB) throw new Error("expected two Edit buttons");
  await user.click(editA);
  expect((screen.getByLabelText(/^name$/i) as HTMLInputElement).value).toBe("github");

  await user.click(editB);
  const nameField = screen.getByLabelText(/^name$/i) as HTMLInputElement;
  const endpointField = screen.getByLabelText(/endpoint/i) as HTMLInputElement;
  expect(nameField.value).toBe("gitlab");
  expect(endpointField.value).toBe("https://b");
});

test("a failing servers query renders the error panel, not an empty table", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(json({ error: { message: "boom" } }, 500)),
  );
  renderPage();
  expect(await screen.findByText(/failed to load servers/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
});

test("Retry after a failure refetches and renders the list", async () => {
  const user = userEvent.setup();
  let calls = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(() => {
    calls += 1;
    return calls === 1
      ? Promise.resolve(json({ error: { message: "boom" } }, 500))
      : Promise.resolve(json({ servers: [
          { id: "1", name: "github", description: "", transport: "http", endpointOrCommand: "https://x", version: "", protocolVersion: "", status: "active", hasSecret: false },
        ], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /retry/i }));
  expect(await screen.findByText("github")).toBeInTheDocument();
  expect(screen.queryByText(/failed to load servers/i)).not.toBeInTheDocument();
});

test("a failing delete surfaces its error near the heading (not silently discarded)", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) =>
    init?.method === "DELETE"
      ? Promise.resolve(json({ error: { message: "server is referenced by an entitlement" } }, 409))
      : Promise.resolve(json({ servers: [
          { id: "1", name: "github", description: "", transport: "http", endpointOrCommand: "https://x", version: "", protocolVersion: "", status: "active", hasSecret: false },
        ], limit: 100, nextCursor: "" })),
  );
  renderPage();
  await user.click(await screen.findByRole("button", { name: /^delete /i }));
  expect(await screen.findByText(/server is referenced by an entitlement/)).toBeInTheDocument();
});

test("create surfaces 409 inline", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) =>
    init?.method === "POST"
      ? Promise.resolve(json({ error: { message: "already exists" } }, 409))
      : Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" })),
  );
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new server/i }));
  await user.type(screen.getByLabelText(/^name$/i), "github");
  await user.type(screen.getByLabelText(/endpoint/i), "https://x/mcp");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  expect(await screen.findByText(/already exists/)).toBeInTheDocument();
});

test("create form exposes version + protocolVersion and posts them", async () => {
  const user = userEvent.setup();
  let postBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) => {
    if (init?.method === "POST") {
      postBody = JSON.parse(String(init.body));
      return Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new server/i }));
  await user.type(screen.getByLabelText(/^name$/i), "github");
  await user.type(screen.getByLabelText(/endpoint/i), "https://x/mcp");
  await user.type(screen.getByLabelText(/^version$/i), "1.2.3");
  await user.type(screen.getByLabelText(/protocol version/i), "2025-06-18");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  await vi.waitFor(() => expect(postBody).toBeDefined());
  expect((postBody as Record<string, string>).version).toBe("1.2.3");
  expect((postBody as Record<string, string>).protocolVersion).toBe("2025-06-18");
});

/**
 * fable-audit §7 #14 (per-upstream TLS trust): the portal is the only place
 * an admin can set `tlsCaRef` — there is no other UI or CLI path to it. This
 * proves the field reaches the submitted request body. Like every other
 * portal unit test in this file, it mocks `fetch`, so it does NOT prove the
 * real API accepts a `tlsCaRef` or that the gateway pins the upstream to it
 * — that's covered by Task 2's Go admin-API tests and Tasks 3/4's gateway
 * tests, against the real server.
 */
test("create form exposes tlsCaRef and posts it", async () => {
  const user = userEvent.setup();
  let postBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) => {
    if (init?.method === "POST") {
      postBody = JSON.parse(String(init.body));
      return Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new server/i }));
  await user.type(screen.getByLabelText(/^name$/i), "github");
  await user.type(screen.getByLabelText(/endpoint/i), "https://x/mcp");
  await user.type(screen.getByLabelText(/tls ca ref/i), "vault:pki/internal#ca_pem");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  await vi.waitFor(() => expect(postBody).toBeDefined());
  expect((postBody as Record<string, string>).tlsCaRef).toBe("vault:pki/internal#ca_pem");
});

test("row Edit/Delete buttons carry name-scoped accessible labels", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ servers: [
    { id: "1", name: "github", description: "", transport: "http", endpointOrCommand: "https://x", version: "", protocolVersion: "", status: "active", hasSecret: false },
  ], limit: 100, nextCursor: "" }));
  renderPage();
  await screen.findByText("github");
  expect(screen.getByRole("button", { name: "Edit github" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Delete github" })).toBeInTheDocument();
});

test("reopening the create form after a failed submit clears the stale error", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) =>
    init?.method === "POST"
      ? Promise.resolve(json({ error: { message: "already exists" } }, 409))
      : Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" })),
  );
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new server/i }));
  await user.type(screen.getByLabelText(/^name$/i), "github");
  await user.type(screen.getByLabelText(/endpoint/i), "https://x/mcp");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  expect(await screen.findByText(/already exists/)).toBeInTheDocument();
  // reopening the form must not carry the prior submit's error
  await user.click(screen.getByRole("button", { name: /new server/i }));
  expect(screen.queryByText(/already exists/)).not.toBeInTheDocument();
});
