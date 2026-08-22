/**
 * The dormant-grant warning on the artifact edit form.
 *
 * An artifact's per-role grants are not deleted when it is switched to org
 * visibility. They go dormant, because only role-visibility artifacts are
 * distributed through entitlements, and switching back to role revives every
 * one of them at once. An admin narrowing access months later can therefore
 * silently restore a list of roles they believed was gone, which is the moment
 * this warning exists for: before the save, on the form where the flip happens.
 *
 * The count is SERVER-derived (`roleGrants` on GET /v1/admin/artifacts/{id}).
 * The second test below is the one that pins that: it serves a count larger
 * than the name list, which is exactly what a truncated response looks like,
 * and a component deriving the number from `roles.length` renders the wrong
 * one. Counting client-side from GET /v1/admin/artifact-entitlements would fail
 * the same way for real, because that list is capped at 100 rows and would
 * undercount on the artifacts with the most grants.
 *
 * Mocks fetch, like every other portal unit test here, so it proves what the
 * form does with a given payload, not that the API sends one. The Go side owns
 * that half (TestArtifactGetReportsRoleGrants).
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ArtifactsPage from "./ArtifactsPage";

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
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const publishStatus = { lastAttemptAt: null, lastSuccessAt: null, lastCommit: "abc123", lastError: "" };

const listRow = {
  id: "9",
  type: "skill" as const,
  name: "shared-skill",
  description: "d",
  content: "",
  memoryScope: null,
  memorySeed: null,
  version: "1.0.0",
  approvalState: "draft" as const,
  approved: false,
  visibility: "org" as const,
  rowVersion: 4,
};

/** The by-id payload the edit form actually prefills from. */
function fullArtifact(overrides: Record<string, unknown> = {}) {
  return { ...listRow, content: "body", ...overrides };
}

// listOverride is typed as a loose record, not inferred from `listRow`: the
// inferred type pins `visibility` to the literal "org" and rejects the
// role-visibility rows two of the tests below need.
function renderPage(byId: Record<string, unknown>, listOverride: Record<string, unknown> = listRow) {
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    calls.push(url);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.endsWith("/v1/admin/artifacts/9")) return Promise.resolve(json(byId));
    return Promise.resolve(json({ artifacts: [listOverride], limit: 100, nextCursor: "" }));
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <ArtifactsPage />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
  return { calls };
}

beforeEach(() => vi.restoreAllMocks());

/** Opens the edit form and returns the visibility select. */
async function openEditForm(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: /^edit shared-skill$/i }));
  return (await screen.findByLabelText(/visibility/i)) as HTMLSelectElement;
}

test("switching an org artifact back to role warns how many grants revive, and for which roles", async () => {
  const user = userEvent.setup();
  renderPage(fullArtifact({ roleGrants: { count: 2, roles: ["platform", "security"], truncated: false } }));

  const visibility = await openEditForm(user);
  // Before the switch there is nothing to warn about: the grants are dormant
  // and staying that way.
  expect(screen.queryByRole("status")).toBeNull();

  await user.selectOptions(visibility, "role");

  const warning = await screen.findByRole("status");
  expect(warning.textContent).toContain("2 dormant grants");
  expect(warning.textContent).toContain("platform, security");
});

test("the revived count is the server's, not the length of the role list it sent", async () => {
  const user = userEvent.setup();
  // A truncated response: 60 grants exist, 2 names are shown. A component
  // counting `roles.length` renders 2 and understates the blast radius by 58 on
  // exactly the artifact where it matters most.
  renderPage(fullArtifact({ roleGrants: { count: 60, roles: ["alpha", "beta"], truncated: true } }));

  await user.selectOptions(await openEditForm(user), "role");

  const warning = await screen.findByRole("status");
  expect(warning.textContent).toContain("60 dormant grants");
  expect(warning.textContent).toContain("and more");
});

test("one grant reads as one grant", async () => {
  const user = userEvent.setup();
  renderPage(fullArtifact({ roleGrants: { count: 1, roles: ["platform"], truncated: false } }));

  await user.selectOptions(await openEditForm(user), "role");

  expect((await screen.findByRole("status")).textContent).toContain("1 dormant grant on this artifact");
});

test("no warning when there is nothing dormant to revive", async () => {
  const user = userEvent.setup();
  renderPage(fullArtifact({ roleGrants: { count: 0, roles: [], truncated: false } }));

  await user.selectOptions(await openEditForm(user), "role");

  expect(screen.queryByRole("status")).toBeNull();
});

test("no warning in the other direction: role to org takes access away, it does not restore any", async () => {
  const user = userEvent.setup();
  const roleRow = { ...listRow, visibility: "role" as const };
  renderPage(
    fullArtifact({ visibility: "role", roleGrants: { count: 3, roles: ["a", "b", "c"], truncated: false } }),
    roleRow,
  );

  const visibility = await openEditForm(user);
  expect(visibility.value).toBe("role");
  await user.selectOptions(visibility, "org");

  expect(screen.queryByRole("status")).toBeNull();
});

test("no warning on an artifact that is ALREADY role-visibility: nothing is being revived", async () => {
  const user = userEvent.setup();
  const roleRow = { ...listRow, visibility: "role" as const };
  renderPage(
    fullArtifact({ visibility: "role", roleGrants: { count: 3, roles: ["a", "b", "c"], truncated: false } }),
    roleRow,
  );

  // Editing anything else on a role artifact whose grants are already LIVE must
  // say nothing: those roles receive it today and will still receive it after
  // the save. Without this case, dropping the `initial.visibility === "org"`
  // half of the condition passes the whole file (measured), and the form warns
  // "saving revives 3 grants" on every edit of every role artifact, about a
  // change that is not happening.
  const visibility = await openEditForm(user);
  expect(visibility.value).toBe("role");
  await user.type(await screen.findByLabelText(/description/i), "!");

  expect(screen.queryByRole("status")).toBeNull();
});

test("the warning needs no second request: it comes from the by-id fetch the form already makes", async () => {
  const user = userEvent.setup();
  const { calls } = renderPage(
    fullArtifact({ roleGrants: { count: 2, roles: ["platform", "security"], truncated: false } }),
  );

  await user.selectOptions(await openEditForm(user), "role");
  await screen.findByRole("status");

  const entitlementCalls = calls.filter((u) => u.includes("artifact-entitlements"));
  expect(entitlementCalls).toEqual([]);
});
