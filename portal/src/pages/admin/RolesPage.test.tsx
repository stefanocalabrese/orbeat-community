import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import RolesPage from "./RolesPage";

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
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <RolesPage />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("lists roles", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json({
      roles: [
        { id: "1", name: "orbeat-user" },
        { id: "2", name: "orbeat-admin" },
      ],
      limit: 100,
      nextCursor: "",
    }),
  );
  renderPage();
  // The page's own copy mentions "orbeat-user" as an example (e.g. `orbeat-user`),
  // so scope the assertion to the role list itself rather than a page-wide text match.
  const list = await screen.findByRole("list");
  expect(within(list).getByText("orbeat-user")).toBeInTheDocument();
  expect(within(list).getByText("orbeat-admin")).toBeInTheDocument();
});

test("a failing roles query renders the error panel, not an empty list", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(json({ error: { message: "boom" } }, 500)),
  );
  renderPage();
  expect(await screen.findByText(/failed to load roles/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
});

test("create submits and clears the input", async () => {
  const user = userEvent.setup();
  const fetchSpy = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation((_input, init) =>
      init?.method === "POST"
        ? Promise.resolve(json({ id: "3", name: "new-role" }, 201))
        : Promise.resolve(json({ roles: [], limit: 100, nextCursor: "" })),
    );
  renderPage();
  const input = await screen.findByLabelText("Role name");
  await user.type(input, "new-role");
  await user.click(screen.getByRole("button", { name: /create/i }));

  await screen.findByDisplayValue("");
  const post = fetchSpy.mock.calls.find(([, init]) => init?.method === "POST");
  expect(post, "a POST should have been sent").toBeDefined();
  expect(String(post![0])).toMatch(/roles/); // hit the roles endpoint, not some other
  expect(String(post![1]?.body)).toContain("new-role"); // with the typed name in the body
  expect(input).toHaveValue("");
});

test("create surfaces 409 inline", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) =>
    init?.method === "POST"
      ? Promise.resolve(json({ error: { message: "already exists" } }, 409))
      : Promise.resolve(json({ roles: [], limit: 100, nextCursor: "" })),
  );
  renderPage();
  const input = await screen.findByLabelText("Role name");
  await user.type(input, "orbeat-user");
  await user.click(screen.getByRole("button", { name: /create/i }));

  expect(await screen.findByText(/already exists/)).toBeInTheDocument();
});

// Delete (fable-audit §7 #10, docs/plans/orbeat-role-deletion-2026-08-11.md
// Task 4). Mocking `fetch` here proves the confirm-gating logic and the
// exact request shape — it CANNOT prove the real client<->server seam (CORS,
// the actual 200 body, or that the cascade really revoked the entitlements).
// portal/e2e/roles.spec.ts (Task 5) is the gate that drives a real browser
// against the real API and asserts via the API that the role and both its
// grants are gone.
//
// The two seeded roles deliberately share a name PREFIX ("contractors" /
// "contractors-eu") to prove the per-row aria-label is unambiguous: RTL's
// string-based `name` matcher below does whole-string equality, not
// substring, so "Delete contractors" reaches only the "contractors" row.
// This is the same defect class as v1.18.0's e2e regression, where
// Playwright's SUBSTRING `getByLabel` matched two rows sharing a name
// prefix — picking colliding names here means a future substring-matching
// consumer (e.g. a Playwright spec) would be exercised against exactly the
// case that broke before, not a case that happens to avoid it.
const rolesWithSharedPrefix = [
  { id: "role-1", name: "contractors" },
  { id: "role-2", name: "contractors-eu" },
];

test("delete: confirming issues DELETE for the confirmed role's id, exactly once", async () => {
  const user = userEvent.setup();
  const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
  const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) =>
    init?.method === "DELETE"
      ? Promise.resolve(json({ entitlementsRevoked: 1, artifactEntitlementsRevoked: 1 }))
      : Promise.resolve(json({ roles: rolesWithSharedPrefix, limit: 100, nextCursor: "" })),
  );
  renderPage();
  await screen.findByText("contractors");

  await user.click(screen.getByRole("button", { name: "Delete contractors" }));

  expect(confirmSpy).toHaveBeenCalledWith(
    'Delete role "contractors"? This also revokes every MCP server and artifact entitlement granted to it.',
  );
  // Exactly once: v1.23.0 found a Reload button defaulting to type="submit"
  // and resubmitting a stale PUT, caught ONLY by asserting call count rather
  // than merely "was called". toHaveLength pins the count, not just presence.
  const deleteCalls = fetchSpy.mock.calls.filter(([, init]) => init?.method === "DELETE");
  expect(deleteCalls).toHaveLength(1);
  expect(String(deleteCalls[0]![0])).toMatch(/\/v1\/admin\/roles\/role-1$/);
});

test("delete: dismissing the confirm issues no request", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(false);
  const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) =>
    init?.method === "DELETE"
      ? Promise.reject(new Error("DELETE must not be issued when the confirm is dismissed"))
      : Promise.resolve(json({ roles: rolesWithSharedPrefix, limit: 100, nextCursor: "" })),
  );
  renderPage();
  await screen.findByText("contractors");

  await user.click(screen.getByRole("button", { name: "Delete contractors" }));

  const deleteCalls = fetchSpy.mock.calls.filter(([, init]) => init?.method === "DELETE");
  expect(deleteCalls).toHaveLength(0);
});

test("delete: surfaces the revoked counts the response returned", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) =>
    init?.method === "DELETE"
      ? Promise.resolve(json({ entitlementsRevoked: 2, artifactEntitlementsRevoked: 1 }))
      : Promise.resolve(json({ roles: rolesWithSharedPrefix, limit: 100, nextCursor: "" })),
  );
  renderPage();
  await screen.findByText("contractors");

  await user.click(screen.getByRole("button", { name: "Delete contractors" }));

  expect(await screen.findByText(/revoked 2 MCP server entitlements and 1 artifact entitlement/)).toBeInTheDocument();
});

test("a failing delete surfaces its error, not a silent no-op", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) =>
    init?.method === "DELETE"
      ? Promise.resolve(json({ error: { message: "role is referenced" } }, 409))
      : Promise.resolve(json({ roles: rolesWithSharedPrefix, limit: 100, nextCursor: "" })),
  );
  renderPage();
  await screen.findByText("contractors");

  await user.click(screen.getByRole("button", { name: "Delete contractors" }));

  expect(await screen.findByText(/role is referenced/)).toBeInTheDocument();
});
