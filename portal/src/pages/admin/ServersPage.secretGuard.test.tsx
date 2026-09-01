/**
 * Defect 1 (2026-09-01, BREAKING): editing an MCP server always prefills
 * Secret ref/TLS CA ref BLANK (the API never echoes either — hasSecret/
 * hasTlsCa flags only). Before this fix the PUT was a naive full-replace, so
 * an omitted key and an explicit "" were byte-identical on the wire and BOTH
 * silently wiped the stored reference — the portal's ORIGINAL B12 guard
 * (this file, pre-fix) worked around that by BLOCKING submission until the
 * admin explicitly confirmed a clear. The API now distinguishes "omitted"
 * (leave unchanged) from "explicit empty string" (clear), so this file now
 * proves: leaving a field blank is safe by default (no block, PUT omits the
 * key), and the still-present "clear" checkbox is what an admin ticks to
 * actively remove a configured reference.
 *
 * Mocks fetch, like every other portal unit test in this repo — see this
 * file's report for what that can't prove about the real PUT.
 */
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

const secretServer = {
  id: "1", name: "github", description: "", transport: "http", endpointOrCommand: "https://x",
  version: "", protocolVersion: "", status: "active", hasSecret: true, hasTlsCa: true, rowVersion: 3,
};
const publicServer = {
  id: "2", name: "public-one", description: "", transport: "http", endpointOrCommand: "https://y",
  version: "", protocolVersion: "", status: "active", hasSecret: false, hasTlsCa: false, rowVersion: 1,
};

function renderPage(fetchImpl: typeof globalThis.fetch) {
  vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ServersPage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("saving an edit with both refs left blank is NOT blocked and OMITS both keys from the PUT body", async () => {
  const user = userEvent.setup();
  let putBody: Record<string, unknown> | undefined;
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      putBody = JSON.parse(String(init?.body));
      return Promise.resolve(json(secretServer));
    }
    return Promise.resolve(json({ servers: [secretServer], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit /i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await vi.waitFor(() => expect(putBody).toBeDefined());
  expect(putBody).not.toHaveProperty("secretRef");
  expect(putBody).not.toHaveProperty("tlsCaRef");
});

test("checking the secret ref clear box sends an explicit empty string, leaving TLS CA untouched", async () => {
  const user = userEvent.setup();
  let putBody: Record<string, unknown> | undefined;
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      putBody = JSON.parse(String(init?.body));
      return Promise.resolve(json(secretServer));
    }
    return Promise.resolve(json({ servers: [secretServer], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit /i }));
  await user.click(
    screen.getByRole("checkbox", { name: /check this box to clear it.*existing secret/i }),
  );
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await vi.waitFor(() => expect(putBody).toBeDefined());
  expect(putBody?.secretRef).toBe("");
  expect(putBody).not.toHaveProperty("tlsCaRef");
});

test("checking the TLS CA clear box sends an explicit empty string, leaving secret ref untouched", async () => {
  const user = userEvent.setup();
  let putBody: Record<string, unknown> | undefined;
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      putBody = JSON.parse(String(init?.body));
      return Promise.resolve(json(secretServer));
    }
    return Promise.resolve(json({ servers: [secretServer], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit /i }));
  await user.click(
    screen.getByRole("checkbox", { name: /check this box to clear it.*existing tls ca/i }),
  );
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await vi.waitFor(() => expect(putBody).toBeDefined());
  expect(putBody?.tlsCaRef).toBe("");
  expect(putBody).not.toHaveProperty("secretRef");
});

test("re-entering a new secret (not leaving it blank) sends the new value and hides the clear checkbox", async () => {
  const user = userEvent.setup();
  let putBody: Record<string, unknown> | undefined;
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      putBody = JSON.parse(String(init?.body));
      return Promise.resolve(json(secretServer));
    }
    return Promise.resolve(json({ servers: [secretServer], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit /i }));
  await user.type(screen.getByLabelText(/^secret ref$/i), "env:ORBEAT_UPSTREAM_NEW");
  await user.type(screen.getByLabelText(/tls ca ref/i), "vault:pki/internal#ca_pem");
  // Typing a value hides the clear checkbox for that field (secretWouldClear
  // derives from the field being blank).
  expect(screen.queryByRole("checkbox", { name: /existing secret/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox", { name: /existing tls ca/i })).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await vi.waitFor(() => expect(putBody).toBeDefined());
  expect(putBody?.secretRef).toBe("env:ORBEAT_UPSTREAM_NEW");
  expect(putBody?.tlsCaRef).toBe("vault:pki/internal#ca_pem");
});

test("a server with no secret or TLS CA configured saves normally with no clear checkbox at all", async () => {
  const user = userEvent.setup();
  let putCount = 0;
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      putCount += 1;
      return Promise.resolve(json(publicServer));
    }
    return Promise.resolve(json({ servers: [publicServer], limit: 100, nextCursor: "" }));
  });

  await user.click(await screen.findByRole("button", { name: /^edit /i }));
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  await vi.waitFor(() => expect(putCount).toBe(1));
});

test("the preserve/clear explanation renders BEFORE the Save button, not after it", async () => {
  renderPage(() => Promise.resolve(json({ servers: [secretServer], limit: 100, nextCursor: "" })));
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /^edit /i }));

  const note = screen.getByText(/leaving either\s*blank now preserves it/i);
  const save = screen.getByRole("button", { name: /^save$/i });
  // DOCUMENT_POSITION_FOLLOWING on the comparison target means `save` comes
  // AFTER `note` in document order.
  expect(note.compareDocumentPosition(save) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});
