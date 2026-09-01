/**
 * Task 8 (spec §9.2): the admin minimum-revision floor's portal surface.
 *
 * PUT /v1/admin/artifacts/{id}/min-revision (internal/api/admin_artifact_min_revision.ee.go)
 * takes an absolute revision number and a required, quoted If-Match. This
 * file covers the happy paths: setting the floor from a revision row,
 * clearing it from the artifact table row, the confirm-cancel path, and the
 * artifact-row marker that makes the floor visible without opening Version
 * history. The 412 path is ArtifactsPage.minRevision.conflict.test.tsx.
 *
 * open-points.md's pinning row, points 6 and 7, closed here:
 *
 *   - Point 7: the Version-history panel's OWN "Remove the floor" button is
 *     GONE. It only ever rendered on the RevisionItem row matching the
 *     floor, so once ORBEAT_ARTIFACT_REVISION_KEEP pruned that revision out
 *     of the panel's list, no row was left to render it on and the floor
 *     could not be cleared from the panel at all. The artifact table row's
 *     own marker reads artifact.minRevision directly, so it is correct even
 *     when the revision is gone, and it is now the ONLY clear path; see
 *     ArtifactsPage.tsx's own comments at both removal sites.
 *   - Point 6: the portal is edition-blind without GET /v1/me's new
 *     "features" object (internal/api/me.go). Every test below except the
 *     last two mocks `features: { pinning: true }`, matching this repo's own
 *     Enterprise build, so the existing happy-path assertions keep proving
 *     what they always proved. The last test proves the OTHER edition: with
 *     `pinning: false`, every floor control disappears from both surfaces.
 *
 * Mocks fetch, like every other portal unit test in this repo. It is
 * evidence the PORTAL wires the field and the route correctly, never that
 * the real API serves `minRevision`/`features` or that the routes exist
 * (Task 9's job for `minRevision`, C7; the real-server proof for `features`
 * is internal/api/me.ee_test.go + internal/communitygen's generated-tree
 * gate).
 */
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ArtifactsPage from "./ArtifactsPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin", "orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const publishStatus = { lastAttemptAt: null, lastSuccessAt: null, lastCommit: "abc123", lastError: "" };

// This repo's own build: GET /v1/me reports pinning on, matching the real
// Enterprise server every other test in this file stands in for.
const meOn = { subject: "boss", email: "b@x", roles: ["orbeat-admin", "orbeat-user"], features: { pinning: true } };

const revisions = [
  { revision: 2, source: "approval", content: "V2", approvedBy: "bob", approvedAt: "2026-07-12T10:00:00Z", isCurrent: true },
  { revision: 1, source: "approval", content: "V1", approvedBy: "bob", approvedAt: "2026-07-12T09:00:00Z", isCurrent: false },
];

function renderPage(fetchImpl: typeof globalThis.fetch) {
  vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ArtifactsPage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());

test("history: Require this or newer PUTs the CLICKED row's own revision and a fresh If-Match, not a constant", async () => {
  // Review finding on the first version of this test: it clicked row #1
  // and asserted minRevision:1, so a mutant that hardcoded
  // `floor.mutate({ minRevision: 1, ... })` regardless of which row was
  // clicked passed anyway (`npx vitest run src/pages/admin/` stayed
  // 18/18 files, 116/116 tests green under that mutant). The row clicked
  // and the value asserted were the same number by construction, so the
  // test could not tell "sends the row I clicked" from "always sends 1".
  //
  // Fixed by clicking TWO different rows in one test and asserting two
  // different bodies AND two different If-Match values (the second one
  // only exists because the first PUT's success changed the artifact's
  // rowVersion): no single hardcoded minRevision or hardcoded If-Match
  // string in the component could satisfy both assertions at once.
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  const v0 = {
    id: "h1", type: "skill" as const, name: "hist", description: "d", content: "c",
    memoryScope: null, memorySeed: null, version: "1.0.0",
    approvalState: "approved" as const, approved: true, visibility: "org" as const,
    rowVersion: 47, minRevision: 0,
  };
  const v1 = { ...v0, minRevision: 2, rowVersion: 48 };
  const v2 = { ...v0, minRevision: 1, rowVersion: 49 };
  const states = [v0, v1, v2];
  let listCalls = 0;
  const puts: { body: unknown; ifMatch: string | null }[] = [];
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    if (method === "PUT" && url.endsWith("/h1/min-revision")) {
      puts.push({ body: JSON.parse(String(init?.body)), ifMatch: new Headers(init?.headers).get("If-Match") });
      return Promise.resolve(json(puts.length === 1 ? v1 : v2));
    }
    listCalls += 1;
    return Promise.resolve(json({ artifacts: [states[Math.min(listCalls - 1, states.length - 1)]], limit: 100, nextCursor: "" }));
  });

  const tr = (await screen.findByText("hist")).closest("tr") as HTMLElement;
  await user.click(within(tr).getByRole("button", { name: /^history for/i }));

  // First click: row #2, with the ORIGINAL rowVersion (47).
  const row2 = (await screen.findByText("#2")).closest("li") as HTMLElement;
  await user.click(within(row2).getByRole("button", { name: /require this or newer/i }));

  expect(window.confirm).toHaveBeenLastCalledWith(
    "Require revision #2 or newer for this artifact? Machines pinned below #2 will receive #2 on their next sync.",
  );
  await vi.waitFor(() => expect(puts).toHaveLength(1));
  expect(puts[0]).toEqual({ body: { minRevision: 2 }, ifMatch: '"47"' });

  // Success invalidates the list; the row picks up the new floor and, more
  // importantly, the new rowVersion (48) the NEXT click must send.
  await screen.findByText("floor #2");

  // Second click: row #1, with the REFRESHED rowVersion (48). If the
  // component ever hardcoded either the revision number or the If-Match
  // string, this assertion is the one that would still fail even though
  // the first one above passed.
  const row1 = (await screen.findByText("#1")).closest("li") as HTMLElement;
  await user.click(within(row1).getByRole("button", { name: /require this or newer/i }));

  expect(window.confirm).toHaveBeenLastCalledWith(
    "Require revision #1 or newer for this artifact? Machines pinned below #1 will receive #1 on their next sync.",
  );
  await vi.waitFor(() => expect(puts).toHaveLength(2));
  expect(puts[1]).toEqual({ body: { minRevision: 1 }, ifMatch: '"48"' });

  await screen.findByText("floor #1");
});

test("history: declining the confirm prompt never sends the PUT", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(false);
  const noFloor = {
    id: "h2", type: "skill" as const, name: "hist2", description: "d", content: "c",
    memoryScope: null, memorySeed: null, version: "1.0.0",
    approvalState: "approved" as const, approved: true, visibility: "org" as const,
    rowVersion: 5, minRevision: 0,
  };
  let putCalls = 0;
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    if (method === "PUT" && url.includes("/min-revision")) {
      putCalls += 1;
      return Promise.resolve(json(noFloor));
    }
    return Promise.resolve(json({ artifacts: [noFloor], limit: 100, nextCursor: "" }));
  });

  const tr = (await screen.findByText("hist2")).closest("tr") as HTMLElement;
  await user.click(within(tr).getByRole("button", { name: /^history for/i }));
  const row1 = (await screen.findByText("#1")).closest("li") as HTMLElement;
  await user.click(within(row1).getByRole("button", { name: /require this or newer/i }));

  expect(window.confirm).toHaveBeenCalled();
  expect(putCalls).toBe(0);
});

test("history: the row that IS the floor shows only the marker, with no floor action of its own", async () => {
  // Point 7: the panel's OWN "Remove the floor" button is gone; the table
  // row's Clear button (tested below) is the only clear path now. What
  // remains to prove here is that the floor's own row shows the read-only
  // "floor" pill and NO floor-related button, while every OTHER row still
  // offers "Require this or newer".
  //
  // Red-proven the same way the pre-removal version of this test was:
  // mutating `isFloor` to `artifact.minRevision === artifact.minRevision`
  // (drops the `r.revision` comparison) would make row #1 wrongly grow a
  // "floor" pill AND wrongly lose its "Require this or newer" button; the
  // row1 assertions below are what would catch it.
  const floored = {
    id: "h3", type: "skill" as const, name: "hist3", description: "d", content: "c",
    memoryScope: null, memorySeed: null, version: "1.0.0",
    approvalState: "approved" as const, approved: true, visibility: "org" as const,
    rowVersion: 71, minRevision: 2,
  };
  const user = userEvent.setup();
  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifacts: [floored], limit: 100, nextCursor: "" }));
  });

  const tr = (await screen.findByText("hist3")).closest("tr") as HTMLElement;
  await user.click(within(tr).getByRole("button", { name: /^history for/i }));

  const row2 = (await screen.findByText("#2")).closest("li") as HTMLElement;
  expect(within(row2).getByText("floor")).toBeInTheDocument();
  expect(within(row2).queryByRole("button", { name: /require this or newer/i })).not.toBeInTheDocument();
  expect(within(row2).queryByRole("button", { name: /remove the floor/i })).not.toBeInTheDocument();

  const row1 = (await screen.findByText("#1")).closest("li") as HTMLElement;
  expect(within(row1).queryByText("floor")).not.toBeInTheDocument();
  expect(within(row1).getByRole("button", { name: /require this or newer/i })).toBeInTheDocument();
});

test("the artifact table row shows the floor without opening Version history, and shows nothing when there is no floor", async () => {
  const floored = {
    id: "a1", type: "skill" as const, name: "floored-one", description: "d", content: "c",
    memoryScope: null, memorySeed: null, version: "1.0.0",
    approvalState: "approved" as const, approved: true, visibility: "org" as const,
    rowVersion: 5, minRevision: 3,
  };
  // A second row with a DIFFERENT floor number, in the same render: a
  // marker that hardcoded "floor #3" (plausible, since it is the only
  // floor value most drafts of this fixture would carry) would pass for
  // `floored` and fail here.
  const flooredOther = { ...floored, id: "a3", name: "floored-other", minRevision: 7 };
  const unfloored = {
    id: "a2", type: "skill" as const, name: "plain-one", description: "d", content: "c",
    memoryScope: null, memorySeed: null, version: "1.0.0",
    approvalState: "approved" as const, approved: true, visibility: "org" as const,
    rowVersion: 5, minRevision: 0,
  };
  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [floored, flooredOther, unfloored], limit: 100, nextCursor: "" }));
  });

  await screen.findByText("floored-one");
  expect(screen.getByText("floor #3")).toBeInTheDocument();
  expect(screen.getByText("floor #7")).toBeInTheDocument();
  const plainRow = (await screen.findByText("plain-one")).closest("tr") as HTMLElement;
  expect(within(plainRow).queryByText(/^floor #/)).not.toBeInTheDocument();
  expect(within(plainRow).queryByRole("button", { name: /clear floor/i })).not.toBeInTheDocument();
});

test("the artifact table row's Clear button removes the floor via a confirm + PUT with minRevision:0 and the row's own rowVersion", async () => {
  // Point 7's new control. 83/6, neither a round number nor a value reused
  // elsewhere in this file, so a mutant that sent a plausible constant
  // (e.g. minRevision:1, If-Match:"1") instead of the row's own values
  // would be caught, the same discipline the panel's floor tests already
  // use (see the review note on the "remove the floor" fixture this test's
  // sibling above replaced: rowVersion 71 for the identical reason).
  const user = userEvent.setup();
  const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
  const floored = {
    id: "a9", type: "skill" as const, name: "clearable", description: "d", content: "c",
    memoryScope: null, memorySeed: null, version: "1.0.0",
    approvalState: "approved" as const, approved: true, visibility: "org" as const,
    rowVersion: 83, minRevision: 6,
  };
  const cleared = { ...floored, minRevision: 0, rowVersion: 84 };
  let listCalls = 0;
  let putBody: unknown;
  let putIfMatch: string | null = null;
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (url.endsWith("/v1/me")) return Promise.resolve(json(meOn));
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (method === "PUT" && url.endsWith("/a9/min-revision")) {
      putBody = JSON.parse(String(init?.body));
      putIfMatch = new Headers(init?.headers).get("If-Match");
      return Promise.resolve(json(cleared));
    }
    listCalls += 1;
    return Promise.resolve(json({ artifacts: [listCalls === 1 ? floored : cleared], limit: 100, nextCursor: "" }));
  });

  await screen.findByText("clearable");
  expect(screen.getByText("floor #6")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Clear floor for clearable" }));

  expect(confirmSpy).toHaveBeenCalledWith(
    "Clear the minimum-revision floor on clearable? Machines pinned below the current revision are no longer held once they next sync.",
  );
  await vi.waitFor(() => expect(putBody).toBeDefined());
  expect(putBody).toEqual({ minRevision: 0 });
  expect(putIfMatch).toBe('"83"');

  await vi.waitFor(() => expect(screen.queryByText("floor #6")).not.toBeInTheDocument());
});

test("when GET /v1/me reports pinning: false, every floor control is absent from both the table row and the Version-history panel", async () => {
  // Point 6's red-proof direction: a Community server's GET /v1/admin/artifacts
  // still carries whatever minRevision the DB happens to hold (defensive:
  // in practice it can only ever be 0 there, since the route that sets it
  // above 0 does not exist on that server), but the console must not act as
  // though the control is usable.
  const user = userEvent.setup();
  const floored = {
    id: "h4", type: "skill" as const, name: "commhist", description: "d", content: "c",
    memoryScope: null, memorySeed: null, version: "1.0.0",
    approvalState: "approved" as const, approved: true, visibility: "org" as const,
    rowVersion: 9, minRevision: 2,
  };
  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.endsWith("/v1/me")) {
      return Promise.resolve(
        json({ subject: "boss", email: "b@x", roles: ["orbeat-admin"], features: { pinning: false } }),
      );
    }
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifacts: [floored], limit: 100, nextCursor: "" }));
  });

  const tr = (await screen.findByText("commhist")).closest("tr") as HTMLElement;
  expect(within(tr).queryByText(/^floor #/)).not.toBeInTheDocument();
  expect(within(tr).queryByRole("button", { name: /clear floor/i })).not.toBeInTheDocument();

  await user.click(within(tr).getByRole("button", { name: /^history for/i }));

  const row2 = (await screen.findByText("#2")).closest("li") as HTMLElement;
  expect(within(row2).queryByText("floor")).not.toBeInTheDocument();
  expect(within(row2).queryByRole("button", { name: /require this or newer/i })).not.toBeInTheDocument();
  expect(within(row2).queryByRole("button", { name: /remove the floor/i })).not.toBeInTheDocument();

  // row1 is NOT the floor row, so under the old (pre-features-gate) code it
  // would show "Require this or newer" regardless of edition; this is the
  // assertion that actually proves the gate applies to every row, not only
  // the one isFloor already suppressed the button on for an unrelated
  // reason.
  const row1 = (await screen.findByText("#1", { exact: true })).closest("li") as HTMLElement;
  expect(within(row1).queryByRole("button", { name: /require this or newer/i })).not.toBeInTheDocument();
});
