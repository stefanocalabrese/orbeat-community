import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ArtifactEntitlementsPage from "./ArtifactEntitlementsPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (b: unknown, s = 200) => new Response(JSON.stringify(b), { status: s, headers: { "Content-Type": "application/json" } });

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input);
    if (init?.method === "POST") return Promise.resolve(json({ id: "e2", roleId: "r1", artifactId: "a1" }, 201));
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user" }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/artifacts")) return Promise.resolve(json({ artifacts: [{ id: "a1", name: "sec-skill", visibility: "role", type: "skill", description: "", content: "", memoryScope: null, memorySeed: null, version: "1", status: "active" }], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifactEntitlements: [{ id: "e1", roleId: "r1", artifactId: "a1" }], limit: 100, nextCursor: "" }));
  });
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><ArtifactEntitlementsPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
}

test("lists artifact entitlements with role + artifact names", async () => {
  renderPage();
  expect(await screen.findByText("Artifact entitlements")).toBeInTheDocument();
  expect(await screen.findByText("orbeat-user")).toBeInTheDocument();
  expect(screen.getByText("sec-skill")).toBeInTheDocument();
});

test("a failing entitlements query renders the error panel, not an empty table", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/artifacts")) return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ error: { message: "boom" } }, 500));
  });
  renderPage();
  expect(await screen.findByText(/failed to load artifact entitlements/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
});

test("a failing secondary roles query surfaces its error near the form", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ error: { message: "roles down" } }, 500));
    if (url.includes("/v1/admin/artifacts")) return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifactEntitlements: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  expect(await screen.findByText(/failed to load roles/i)).toBeInTheDocument();
  expect(screen.getByText(/roles down/)).toBeInTheDocument();
});

test("renders the New entitlement button", async () => {
  renderPage();
  expect(await screen.findByRole("button", { name: /new entitlement/i })).toBeInTheDocument();
});

test("no role-visibility artifacts: Create is disabled and an empty-state hint shows", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user" }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/artifacts")) return Promise.resolve(json({ artifacts: [{ id: "a9", name: "org-only", visibility: "org", type: "skill", description: "", content: "", memoryScope: null, memorySeed: null, version: "1", status: "active" }], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifactEntitlements: [], limit: 100, nextCursor: "" }));
  });
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new entitlement/i }));
  expect(screen.getByText(/no role-visibility artifacts/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^create$/i })).toBeDisabled();
});

// B35: `roleArtifacts` is filtered from only the artifacts LOADED so far
// (page 1). When more pages exist, zero role-visibility artifacts on THIS
// page does not mean zero exist — the copy must not claim otherwise.
test("role-visibility artifacts may exist on a later page: the hint says so instead of falsely claiming none exist at all", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user" }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/artifacts")) return Promise.resolve(json({ artifacts: [{ id: "a9", name: "org-only", visibility: "org", type: "skill", description: "", content: "", memoryScope: null, memorySeed: null, version: "1", status: "active" }], limit: 1, nextCursor: "cursor-2" }));
    return Promise.resolve(json({ artifactEntitlements: [], limit: 100, nextCursor: "" }));
  });
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new entitlement/i }));
  expect(screen.queryByText(/no role-visibility artifacts exist yet/i)).not.toBeInTheDocument();
  expect(screen.getByText(/more artifacts exist that have not loaded yet/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^create$/i })).toBeDisabled();
});

test("create posts the selected role + artifact", async () => {
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new entitlement/i }));
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  const post = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.find(([, i]) => i?.method === "POST");
  expect(post).toBeTruthy();
  const body = JSON.parse(post![1]!.body as string);
  expect(body.roleId).toBe("r1");
  expect(body.artifactId).toBe("a1");
});

test("a failing delete surfaces its error near the heading (not silently discarded)", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input);
    if (init?.method === "DELETE") return Promise.resolve(json({ error: { message: "artifact entitlement delete rejected" } }, 409));
    if (url.includes("/v1/admin/roles")) return Promise.resolve(json({ roles: [{ id: "r1", name: "orbeat-user" }], limit: 100, nextCursor: "" }));
    if (url.includes("/v1/admin/artifacts")) return Promise.resolve(json({ artifacts: [{ id: "a1", name: "sec-skill", visibility: "role", type: "skill", description: "", content: "", memoryScope: null, memorySeed: null, version: "1", status: "active" }], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifactEntitlements: [{ id: "e1", roleId: "r1", artifactId: "a1" }], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /^delete$/i }));
  expect(await screen.findByText(/artifact entitlement delete rejected/)).toBeInTheDocument();
});
