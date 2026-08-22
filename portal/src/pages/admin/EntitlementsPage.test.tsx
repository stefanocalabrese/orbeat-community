import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import EntitlementsPage from "./EntitlementsPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (b: unknown, s = 200) => new Response(JSON.stringify(b), { status: s, headers: { "Content-Type": "application/json" } });

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input);
    if (init?.method === "POST") return Promise.resolve(json({ id: "e2", roleId: "r1", mcpServerId: "s1", allowedTools: null, permissions: [] }, 201));
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user" }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/servers")) return Promise.resolve(json({ servers: [{ id: "s1", name: "github", description: "", transport: "http", endpointOrCommand: "x", version: "", protocolVersion: "", status: "active", hasSecret: false }], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ entitlements: [{ id: "e1", roleId: "r1", mcpServerId: "s1", allowedTools: ["echo"], permissions: [] }], limit: 100, nextCursor: "" }));
  });
});

test("lists entitlements with resolved names and tools", async () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><EntitlementsPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
  expect(await screen.findByText("orbeat-user")).toBeInTheDocument();
  expect(screen.getByText("github")).toBeInTheDocument();
  expect(screen.getByText(/echo/)).toBeInTheDocument();
});

test("a failing entitlements query renders the error panel, not an empty table", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/servers")) return Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ error: { message: "boom" } }, 500));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><EntitlementsPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
  expect(await screen.findByText(/failed to load entitlements/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
});

test("a failing secondary roles query surfaces its error near the form", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ error: { message: "roles down" } }, 500));
    if (url.includes("/v1/admin/servers")) return Promise.resolve(json({ servers: [], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ entitlements: [], limit: 100, nextCursor: "" }));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><EntitlementsPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
  expect(await screen.findByText(/failed to load roles/i)).toBeInTheDocument();
  expect(screen.getByText(/roles down/)).toBeInTheDocument();
});

test("blank allowedTools posts null (all tools)", async () => {
  const user = userEvent.setup();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><EntitlementsPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
  await user.click(await screen.findByRole("button", { name: /new entitlement/i }));
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  const post = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.find(([, i]) => i?.method === "POST");
  expect(post).toBeTruthy();
  expect(JSON.parse(post![1]!.body as string).allowedTools).toBeNull();
});

test("a failing delete surfaces its error near the heading (not silently discarded)", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input);
    if (init?.method === "DELETE") return Promise.resolve(json({ error: { message: "entitlement delete rejected" } }, 409));
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user" }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/servers")) return Promise.resolve(json({ servers: [{ id: "s1", name: "github", description: "", transport: "http", endpointOrCommand: "x", version: "", protocolVersion: "", status: "active", hasSecret: false }], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ entitlements: [{ id: "e1", roleId: "r1", mcpServerId: "s1", allowedTools: null, permissions: [] }], limit: 100, nextCursor: "" }));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><EntitlementsPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
  await user.click(await screen.findByRole("button", { name: /^delete$/i }));
  expect(await screen.findByText(/entitlement delete rejected/)).toBeInTheDocument();
});
