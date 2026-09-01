/**
 * The pending-identity indicators on the artifacts page (migration 00016).
 *
 * An artifact's type, name and visibility are snapshotted at approval the way
 * its content already was, so an admin who renames an approved artifact sees
 * the new name here while every developer keeps receiving the old one until a
 * second admin approves the change. Nothing on this page said so before.
 *
 * Every fixture below is ASYMMETRIC on purpose: the distributed identity
 * differs from the live one in all three fields, with values that cannot be
 * mistaken for each other (`skill`/`subagent`, `foo`/`bar`, `org`/`role`).
 * Symmetric fixtures were how an earlier slice shipped a count assertion that
 * stayed green with `max` and `current` swapped, and the same trap is live
 * here: with equal values, a marker reading the LIVE field instead of the
 * approved one, or reading the wrong field entirely, passes.
 *
 * Mocks fetch, like every other portal unit test here, so it proves what the
 * page does with a given payload, not that the API sends one. The Go side owns
 * that half (Task 6's DTO round-trip on the list route), and the API-to-portal
 * seam is Task 8's Playwright spec.
 */
import { render, screen, within } from "@testing-library/react";
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

/**
 * Renamed, retyped and re-scoped since its last approval: live `subagent`
 * `bar` `role`, distributed `skill` `foo` `org`. Every pair is distinguishable,
 * so an indicator reading the wrong side or the wrong field is visible.
 */
const diverged = {
  id: "d1",
  type: "subagent",
  name: "bar",
  description: "d",
  content: "working body",
  memoryScope: null,
  memorySeed: null,
  version: "1.0.0",
  visibility: "role",
  approvalState: "draft",
  approved: true,
  approvedType: "skill",
  approvedName: "foo",
  approvedVisibility: "org",
  rowVersion: 7,
};

/** Same artifact with nothing pending: live identity IS the distributed one. */
const inSync = {
  ...diverged,
  id: "s1",
  type: "skill",
  name: "foo",
  visibility: "org",
  approvalState: "approved",
};

/** Never approved, so the server omits all four approved fields. */
const unapproved = {
  ...diverged,
  id: "u1",
  approved: false,
  approvedType: undefined,
  approvedName: undefined,
  approvedVisibility: undefined,
};

function renderPage(rows: unknown[], extra?: (url: string, init?: RequestInit) => Response | undefined) {
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    const hit = extra?.(url, init);
    if (hit) return Promise.resolve(hit);
    return Promise.resolve(json({ artifacts: rows, limit: 100, nextCursor: "" }));
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
}

/** The three body cells whose value the identity markers sit under. */
async function identityCells(name: string) {
  const row = (await screen.findByText(name)).closest("tr");
  if (!row) throw new Error(`no row for ${name}`);
  const cells = within(row).getAllByRole("cell");
  const [nameCell, typeCell, , visibilityCell] = cells;
  if (!nameCell || !typeCell || !visibilityCell) throw new Error("expected four value cells");
  return { row, nameCell, typeCell, visibilityCell };
}

beforeEach(() => vi.restoreAllMocks());

test("the list marks each changed identity field with what developers still receive", async () => {
  renderPage([diverged]);

  const { row, nameCell, typeCell, visibilityCell } = await identityCells("bar");

  // Per CELL, not per row: a single marker that only ever named the approved
  // NAME would pass a row-wide assertion while saying nothing about the type
  // change that moves the file from agents/ to skills/.
  expect(within(nameCell).getByRole("note").textContent).toBe("distributing as foo");
  expect(within(typeCell).getByRole("note").textContent).toBe("distributing as skill");
  expect(within(visibilityCell).getByRole("note").textContent).toBe("distributing as org");
  // Exactly three: no marker leaks onto the State column, and none is
  // rendered twice.
  expect(within(row).getAllByRole("note")).toHaveLength(3);
});

test("no marker on an artifact whose live identity is the one being distributed", async () => {
  renderPage([inSync]);

  const { row } = await identityCells("foo");
  expect(within(row).queryAllByRole("note")).toEqual([]);
});

test("no marker on an artifact that has never been approved: nothing is distributed to contradict", async () => {
  // The three approved fields are absent, which is what the server sends when
  // there is no snapshot. A marker here would have to compare against
  // `undefined` and would announce a distributed identity that does not exist.
  renderPage([unapproved]);

  const { row } = await identityCells("bar");
  expect(within(row).queryAllByRole("note")).toEqual([]);
});

/**
 * Opens the edit form for `name` and returns it.
 *
 * Scoped to the `<form>`, not to the page: the table's own per-cell markers
 * are still on screen behind the form, so an unscoped `getByRole("note")`
 * either matches several or, worse, matches a table marker and reports the
 * form as correct when it renders nothing at all.
 */
async function openEditForm(name: string, rows: unknown[], byId: Record<string, unknown>) {
  const user = userEvent.setup();
  renderPage(rows, (url, init) =>
    init?.method === "GET" && url.endsWith(`/artifacts/${String(byId["id"])}`) ? json(byId) : undefined,
  );
  await user.click(await screen.findByRole("button", { name: new RegExp(`^edit ${name}$`, "i") }));
  const form = (await screen.findByLabelText(/^name$/i)).closest("form");
  if (!form) throw new Error("edit form not rendered");
  return form;
}

test("the edit form names every field developers still receive", async () => {
  const form = await openEditForm("bar", [diverged], diverged);

  expect(within(form).getByRole("note").textContent).toContain(
    "Developers still receive type skill, name foo, visibility org until the saved changes are distributed",
  );
});

test("no note on the edit form of an artifact with nothing pending, while a diverged row still shows its own", async () => {
  // Both rows are listed, so the page is NOT free of notes: the three markers
  // on `bar` are there throughout. A form that rendered the note
  // unconditionally would be caught here, and so would a test that forgot to
  // scope its query to the form.
  const form = await openEditForm("foo", [diverged, inSync], inSync);

  expect(within(form).queryByRole("note")).toBeNull();
  expect(screen.getAllByRole("note")).toHaveLength(3);
});

/**
 * Opens the version-history panel for `name`, with window.confirm stubbed to
 * DECLINE. Declining is the point: it lets the assertions read the text the
 * admin was shown without the rollback firing, so what is being tested is the
 * warning rather than the mutation behind it.
 */
async function openHistory(name: string, revisions: unknown[], row: unknown) {
  const user = userEvent.setup();
  const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
  renderPage([row], (url) =>
    url.includes("/revisions") ? json({ revisions, limit: 100, nextCursor: "" }) : undefined,
  );
  const tr = (await screen.findByText(name)).closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  await user.click(within(tr).getByRole("button", { name: /^history for/i }));
  return { user, confirmText: () => confirmSpy.mock.calls[0]?.[0] ?? "" };
}

test("rolling back to a revision with a different identity says what it moves, from and to", async () => {
  // #2 froze the identity currently distributed; #1 froze a different one, so
  // rolling back to it renames the file on every entitled machine.
  const revisions = [
    { revision: 2, source: "approval", content: "V2", type: "skill", name: "foo", visibility: "org", approvedBy: "bob", approvedAt: "2026-08-20T10:00:00Z", isCurrent: true },
    { revision: 1, source: "approval", content: "V1", type: "rule", name: "legacy", visibility: "role", approvedBy: "bob", approvedAt: "2026-08-19T10:00:00Z", isCurrent: false },
  ];
  const { user, confirmText } = await openHistory("bar", revisions, diverged);

  await user.click(await screen.findByRole("button", { name: /roll back to #1/i }));

  const msg = confirmText();
  expect(msg).toContain("Roll distribution back to revision #1?");
  // Each pair reads distributed-today first, target second. Asserting both
  // sides is what makes a reversed message fail.
  expect(msg).toContain("type skill → rule");
  expect(msg).toContain("name foo → legacy");
  expect(msg).toContain("visibility org → role");
});

test("rolling back to a revision that froze the identity already live says the identity does not change", async () => {
  const revisions = [
    { revision: 2, source: "approval", content: "V2", type: "skill", name: "foo", visibility: "org", approvedBy: "bob", approvedAt: "2026-08-20T10:00:00Z", isCurrent: true },
    { revision: 1, source: "approval", content: "V1", type: "skill", name: "foo", visibility: "org", approvedBy: "bob", approvedAt: "2026-08-19T10:00:00Z", isCurrent: false },
  ];
  const { user, confirmText } = await openHistory("bar", revisions, diverged);

  await user.click(await screen.findByRole("button", { name: /roll back to #1/i }));

  const msg = confirmText();
  expect(msg).toContain("It keeps being distributed as type skill, name foo, visibility org");
  expect(msg).not.toContain("→");
});

test("rolling back to a pre-00016 revision says the OPPOSITE explicitly: the name does not move", async () => {
  // Silence would read as "no rename", which happens to be true here, and
  // would read identically if the fields were simply missing from a response
  // that should have carried them.
  const revisions = [
    { revision: 2, source: "approval", content: "V2", type: "skill", name: "foo", visibility: "org", approvedBy: "bob", approvedAt: "2026-08-20T10:00:00Z", isCurrent: true },
    { revision: 1, source: "approval", content: "V1", approvedBy: "bob", approvedAt: "2026-07-01T10:00:00Z", isCurrent: false },
  ];
  const { user, confirmText } = await openHistory("bar", revisions, diverged);

  await user.click(await screen.findByRole("button", { name: /roll back to #1/i }));

  const msg = confirmText();
  expect(msg).toContain("Revision #1 recorded no identity, so only the content reverts");
  // The identity it keeps is the DISTRIBUTED one, `foo`, not the live `bar`
  // the admin can see in the table above the panel.
  expect(msg).toContain("It keeps being distributed as type skill, name foo, visibility org");
  expect(msg).not.toContain("bar");
});

test("on a withdrawn artifact the kept identity falls back to the live row, mirroring the server's COALESCE", async () => {
  // Withdraw clears approved_content, and the 00016 CHECK clears the approved
  // identity with it. store.RollbackArtifact falls back to COALESCE(approved_*,
  // live_*) there; a confirmation that named `undefined` would describe an
  // outcome the rollback does not produce.
  const withdrawn = { ...unapproved, name: "bar", type: "subagent", visibility: "role" };
  const revisions = [
    { revision: 1, source: "approval", content: "V1", approvedBy: "bob", approvedAt: "2026-07-01T10:00:00Z", isCurrent: false },
  ];
  const { user, confirmText } = await openHistory("bar", revisions, withdrawn);

  await user.click(await screen.findByRole("button", { name: /roll back to #1/i }));

  expect(confirmText()).toContain("It keeps being distributed as type subagent, name bar, visibility role");
});
