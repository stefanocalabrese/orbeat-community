import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../auth/useAuth";
import AdminLayout from "./AdminLayout";

const boss = {
  isLoading: false,
  authenticated: true,
  roles: ["orbeat-admin"],
  token: "t",
  login: () => {},
  logout: () => {},
  subject: "boss",
  email: "b@x",
};
const json = (b: unknown, s = 200) =>
  new Response(JSON.stringify(b), { status: s, headers: { "Content-Type": "application/json" } });

function renderLayout(fetchImpl: typeof globalThis.fetch, initialEntries: string[] = ["/admin/servers"]) {
  vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={initialEntries}>
          <AdminLayout />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("renders the Console label and all nav links with correct hrefs when virtual keys are enabled", async () => {
  renderLayout(() =>
    Promise.resolve(json({ subject: "boss", email: "b@x", roles: ["orbeat-admin"], features: { virtualKeys: true } })),
  );

  expect(screen.getByText("Console")).toBeInTheDocument();

  const links: [string, string][] = [
    ["Servers", "/admin/servers"],
    ["Artifacts", "/admin/artifacts"],
    ["Review queue", "/admin/review"],
    ["Roles", "/admin/roles"],
    ["Entitlements", "/admin/entitlements"],
    ["Artifact entitlements", "/admin/artifact-entitlements"],
    ["Virtual keys", "/admin/virtual-keys"],
    ["Audit", "/admin/audit"],
  ];

  for (const [name, href] of links) {
    expect(await screen.findByRole("link", { name })).toHaveAttribute("href", href);
  }
});

test("marks the current route's nav item active", async () => {
  renderLayout(() => Promise.resolve(json({ features: { virtualKeys: true } })), ["/admin/roles"]);
  // NavLink stamps aria-current="page" on the active link — exercises the
  // isActive branch both ways (Roles active, Servers not).
  expect(await screen.findByRole("link", { name: "Roles" })).toHaveAttribute("aria-current", "page");
  expect(screen.getByRole("link", { name: "Servers" })).not.toHaveAttribute("aria-current");
});

// B35: the Virtual keys nav item used to render UNCONDITIONALLY, so a
// Community admin (features.virtualKeys: false, or a server predating the
// field) could navigate to a page that renders `null` — an empty pane with
// no explanation. Hiding the link at the source is what stops them getting
// there via the console's own navigation in the first place.
test("omits the Virtual keys link on Community (features.virtualKeys: false)", async () => {
  renderLayout(() => Promise.resolve(json({ features: { virtualKeys: false } })));
  await screen.findByRole("link", { name: "Servers" });
  expect(screen.queryByRole("link", { name: "Virtual keys" })).not.toBeInTheDocument();
});

test("omits the Virtual keys link when GET /v1/me fails (fail-closed, same direction useVirtualKeysEnabled already takes)", async () => {
  renderLayout(() => Promise.resolve(json({ error: { message: "boom" } }, 500)));
  await screen.findByRole("link", { name: "Servers" });
  expect(screen.queryByRole("link", { name: "Virtual keys" })).not.toBeInTheDocument();
});
