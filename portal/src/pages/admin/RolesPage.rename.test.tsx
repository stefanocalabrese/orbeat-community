/**
 * Rename (PUT /v1/admin/roles/{id}, docs/plans/orbeat-role-rename-2026-08-27.md
 * Task 6).
 *
 * The headline behaviour under test is the assertion path: renaming in
 * orbeat alone detaches every user from the role (authz.Resolver.Resolve
 * binds a role to the IdP BY NAME), so when no realm-role lookup is
 * configured the API refuses the rename with idpAssertionRequiredCode until
 * the operator explicitly ticks a checkbox stating that consequence. The
 * portal must NEVER send that assertion pre-emptively -- the checkbox must
 * not even exist until the API's 400 asks for it, and it must default
 * unchecked. This suite mocks `fetch`, like every other portal unit test in
 * this repo: it proves the client-side state machine, not the real
 * API↔portal seam -- that is portal/e2e/roles.spec.ts's job.
 */
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
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const roleV5 = { id: "1", name: "contractors", rowVersion: 5 };

/**
 * Spies on `fetch` with `fetchImpl` and renders the page, returning the spy
 * so a test can inspect exactly which requests were made -- the one and only
 * place `vi.spyOn(globalThis, "fetch")` is called in this file, so a test
 * never accidentally double-wraps an already-mocked `fetch` (which silently
 * breaks `.mock.calls` bookkeeping on the outer spy).
 */
function renderPage(fetchImpl: typeof globalThis.fetch) {
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <RolesPage />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
  return spy;
}

beforeEach(() => vi.restoreAllMocks());

/** Parses the JSON body handed to a mocked fetch call. */
function bodyOf(call: [unknown, RequestInit?]): Record<string, unknown> {
  return JSON.parse(String(call[1]?.body)) as Record<string, unknown>;
}

const rolesPage = (roles: unknown[] = [roleV5]) => json({ roles, limit: 100, nextCursor: "" });

test("Rename opens an inline edit with no checkbox: the API has not asked for one yet", async () => {
  const user = userEvent.setup();
  renderPage(() => Promise.resolve(rolesPage()));

  const list = await screen.findByRole("list");
  await user.click(within(list).getByRole("button", { name: "Rename contractors" }));

  expect(screen.getByRole("textbox", { name: "New name for contractors" })).toHaveValue("contractors");
  expect(screen.getByRole("button", { name: /^save$/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^cancel$/i })).toBeInTheDocument();
  // The checkbox is the whole point of this slice: it must not exist before
  // the API has ever refused anything.
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

test("Cancel discards the edit and issues no PUT", async () => {
  const user = userEvent.setup();
  const fetchSpy = renderPage(() => Promise.resolve(rolesPage()));

  await user.click(await screen.findByRole("button", { name: "Rename contractors" }));
  await user.clear(screen.getByRole("textbox", { name: "New name for contractors" }));
  await user.type(screen.getByRole("textbox", { name: "New name for contractors" }), "renamed-co");
  await user.click(screen.getByRole("button", { name: /^cancel$/i }));

  expect(screen.queryByRole("textbox", { name: /new name for/i })).not.toBeInTheDocument();
  const putCalls = fetchSpy.mock.calls.filter(([, init]) => init?.method === "PUT");
  expect(putCalls).toHaveLength(0);
});

test(
  "MANDATORY MUTANT 1: the first submit never carries the assertion, even though the operator has not " +
    "touched a checkbox that does not exist yet; the API's refusal then reveals it, unticked, with the " +
    "consequence stated plainly",
  async () => {
    const user = userEvent.setup();
    const fetchSpy = renderPage((_input, init) => {
      if ((init?.method ?? "GET") === "PUT") {
        return Promise.resolve(
          json(
            {
              error: {
                message:
                  "no realm-role lookup is configured on this deployment; confirm the role was " +
                  "already renamed in the identity provider by resubmitting with idpRenamed=true",
              },
              code: "idp_rename_assertion_required",
            },
            400,
          ),
        );
      }
      return Promise.resolve(rolesPage());
    });

    await user.click(await screen.findByRole("button", { name: "Rename contractors" }));
    await user.clear(screen.getByRole("textbox", { name: "New name for contractors" }));
    await user.type(screen.getByRole("textbox", { name: "New name for contractors" }), "renamed-co");
    await user.click(screen.getByRole("button", { name: /^save$/i }));

    // The checkbox now exists, and it is UNCHECKED by default.
    const checkbox = await screen.findByRole("checkbox");
    expect(checkbox).not.toBeChecked();

    // The API's own message is shown verbatim.
    expect(await screen.findByText(/no realm-role lookup is configured on this deployment/i)).toBeInTheDocument();

    // The consequence is stated in the portal's own copy, not just the API's:
    // by-name matching, and that entitlements survive while access is lost.
    expect(screen.getByText(/orbeat matches roles to the identity provider by name/i)).toBeInTheDocument();
    expect(screen.getByText(/every user holding it loses it immediately/i)).toBeInTheDocument();

    const putCalls = fetchSpy.mock.calls.filter(([, init]) => init?.method === "PUT");
    expect(putCalls).toHaveLength(1);
    expect(bodyOf(putCalls[0]!)).toMatchObject({ name: "renamed-co", idpRenamed: false });
  },
);

test(
  "MANDATORY MUTANT 2: ticking the checkbox and resubmitting sends idpRenamed:true, and each submit " +
    "fires exactly one PUT",
  async () => {
    const user = userEvent.setup();
    let putCalls = 0;
    const fetchSpy = renderPage((_input, init) => {
      if ((init?.method ?? "GET") === "PUT") {
        putCalls += 1;
        if (putCalls === 1) {
          return Promise.resolve(
            json({ error: { message: "confirm it" }, code: "idp_rename_assertion_required" }, 400),
          );
        }
        return Promise.resolve(json({ id: "1", name: "renamed-co", rowVersion: 6 }));
      }
      return Promise.resolve(rolesPage([putCalls >= 2 ? { ...roleV5, name: "renamed-co", rowVersion: 6 } : roleV5]));
    });

    await user.click(await screen.findByRole("button", { name: "Rename contractors" }));
    await user.clear(screen.getByRole("textbox", { name: "New name for contractors" }));
    await user.type(screen.getByRole("textbox", { name: "New name for contractors" }), "renamed-co");
    await user.click(screen.getByRole("button", { name: /^save$/i }));

    const checkbox = await screen.findByRole("checkbox");
    expect(checkbox).not.toBeChecked();
    await user.click(checkbox);
    expect(checkbox).toBeChecked();

    await user.click(screen.getByRole("button", { name: /^save$/i }));

    // Row exits edit mode on success.
    await screen.findByText("renamed-co");
    expect(screen.queryByRole("textbox", { name: /new name for/i })).not.toBeInTheDocument();

    const allPutCalls = fetchSpy.mock.calls.filter(([, init]) => init?.method === "PUT");
    // Exactly two PUTs total: one per Save click, never an extra silent retry.
    expect(allPutCalls).toHaveLength(2);
    expect(bodyOf(allPutCalls[0]!)).toMatchObject({ idpRenamed: false });
    expect(bodyOf(allPutCalls[1]!)).toMatchObject({ name: "renamed-co", idpRenamed: true });
  },
);

test("the PUT carries If-Match built from the row's rowVersion", async () => {
  const user = userEvent.setup();
  const fetchSpy = renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") return Promise.resolve(json({ id: "1", name: "x", rowVersion: 6 }));
    return Promise.resolve(rolesPage());
  });

  await user.click(await screen.findByRole("button", { name: "Rename contractors" }));
  await user.clear(screen.getByRole("textbox", { name: "New name for contractors" }));
  await user.type(screen.getByRole("textbox", { name: "New name for contractors" }), "x");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await vi.waitFor(() =>
    expect(fetchSpy.mock.calls.filter(([, init]) => init?.method === "PUT")).toHaveLength(1),
  );
  const putCalls = fetchSpy.mock.calls.filter(([, init]) => init?.method === "PUT");
  const headers = putCalls[0]![1]?.headers as Record<string, string>;
  expect(headers["If-Match"]).toBe('"5"');
});

test("412 on rename shows the conflict notice, never auto-retries the PUT, and Reload refetches", async () => {
  const user = userEvent.setup();
  let getCalls = 0;
  let putCalls = 0;
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      putCalls += 1;
      return Promise.resolve(json({ error: { message: "row_version mismatch" } }, 412));
    }
    getCalls += 1;
    return Promise.resolve(rolesPage());
  });

  await user.click(await screen.findByRole("button", { name: "Rename contractors" }));
  await user.clear(screen.getByRole("textbox", { name: "New name for contractors" }));
  await user.type(screen.getByRole("textbox", { name: "New name for contractors" }), "renamed-co");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(
    await screen.findByText(/this changed since you loaded it — reload to see the current state/i),
  ).toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  expect(putCalls).toBe(1);

  const getCallsAtConflict = getCalls;
  await user.click(screen.getByRole("button", { name: /reload/i }));
  await vi.waitFor(() => expect(getCalls).toBeGreaterThan(getCallsAtConflict));

  expect(putCalls).toBe(1);
  expect(screen.queryByRole("textbox", { name: /new name for/i })).not.toBeInTheDocument();
});

test("409 name collision renders the API message inline, offering no checkbox", async () => {
  const user = userEvent.setup();
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      return Promise.resolve(json({ error: { message: "a role in this tenant already has this name" } }, 409));
    }
    return Promise.resolve(rolesPage());
  });

  await user.click(await screen.findByRole("button", { name: "Rename contractors" }));
  await user.clear(screen.getByRole("textbox", { name: "New name for contractors" }));
  await user.type(screen.getByRole("textbox", { name: "New name for contractors" }), "orbeat-admin");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(await screen.findByText(/a role in this tenant already has this name/)).toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

test('a configured lookup\'s "no such realm role" 400 renders inline, offering no checkbox either', async () => {
  const user = userEvent.setup();
  renderPage((_input, init) => {
    if ((init?.method ?? "GET") === "PUT") {
      return Promise.resolve(
        json(
          { error: { message: 'no realm role named "ghost-role" exists in the identity provider; rename it there first' } },
          400,
        ),
      );
    }
    return Promise.resolve(rolesPage());
  });

  await user.click(await screen.findByRole("button", { name: "Rename contractors" }));
  await user.clear(screen.getByRole("textbox", { name: "New name for contractors" }));
  await user.type(screen.getByRole("textbox", { name: "New name for contractors" }), "ghost-role");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(await screen.findByText(/no realm role named "ghost-role" exists/i)).toBeInTheDocument();
  // This refusal is not the operator's to override -- verifyIdpRename's
  // "verified_absent" arm never returns idpAssertionRequiredCode.
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});
