import { expect, request as playwrightRequest, test, type APIRequestContext, type Page } from "@playwright/test";

// The artifact minimum-revision floor
// (docs/specs/2026-08-22-orbeat-artifact-version-pinning-design.md sec 9.2,
// internal/api/admin_artifact_min_revision.ee.go).
//
// The floor control shipped in 279402f with jsdom coverage only
// (ArtifactsPage.minRevision.test.tsx), which mocks `fetch`: it proves the
// button sends what the test told the mock to expect, never that the real
// orbeat-api accepts a PUT with a required, quoted If-Match on this route.
// Every other piece of the pinning slice got a real-server check through
// `make smoke` (scripts/smoke.sh's pinning gates); the portal was the one
// place the chain still stopped at a fake, the exact class servers.spec.ts
// and dropdowns.spec.ts exist to close on other admin surfaces (v1.16.0: a
// dropdown offered a status the API no longer accepted, and every portal
// test stayed green because it mocks fetch).
//
// So: drive the control through a real browser against the real stack, then
// assert the floor took effect as the SERVER sees it: a full page reload
// (forces a fresh GET, not the mutation's own optimistic cache write) and an
// out-of-band API read via a separate request context, the same
// "read back independent of the browser" idiom identity-approval.spec.ts uses
// for its load-bearing assertion.
//
// open-points.md's pinning row, points 6 and 7, extended this test rather
// than adding a second file for the same interaction. Point 7: the
// Version-history panel's OWN "Remove the floor" button is gone: it could
// only ever clear a floor whose revision was still in the panel's (paged,
// prunable) list, so it is replaced everywhere by the artifact table row's
// own Clear button, which reads minRevision straight off the row and works
// regardless. This spec's clear step below now drives THAT button. Point 6
// (GET /v1/me's edition-capability gate) needs no separate e2e coverage:
// this stack is Enterprise, so `features.pinning` is true here exactly as it
// always was, and the Community-off case is proven server-side
// (internal/communitygen's generated-tree gate) and in jsdom
// (ArtifactsPage.minRevision.test.tsx), neither of which needs a browser.

async function login(page: Page, user: string, pass: string) {
  await page.goto("/");
  await page.getByRole("button", { name: /sign in/i }).click();
  // Keycloak login page (external origin, localhost:8088)
  await page.fill("#username", user);
  await page.fill("#password", pass);
  await page.click("#kc-login");
  await page.waitForURL(/\/catalog/, { timeout: 30_000 });
  await expect(page.getByRole("heading", { name: /^catalog$/i })).toBeVisible({ timeout: 15_000 });
}

const API_BASE = "http://localhost:8080";
const KC_TOKEN_URL = "http://localhost:8088/realms/orbeat/protocol/openid-connect/token";

/** Direct-grant token, the same mechanism scripts/smoke.sh and identity-approval.spec.ts use. */
async function tokenFor(request: APIRequestContext, user: string, pass: string): Promise<string> {
  const res = await request.post(KC_TOKEN_URL, {
    form: { grant_type: "password", client_id: "orbeat-cli", username: user, password: pass },
  });
  expect(res.ok(), `token request for ${user} failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  const body = (await res.json()) as { access_token?: string };
  expect(body.access_token, `no access_token for ${user}`).toBeTruthy();
  return body.access_token as string;
}

// Per-run unique and all-digit-suffixed (the servers.spec.ts lesson): a fixed
// name would let a second run against a surviving stack pass vacuously on the
// previous run's row, or 409 on create.
const NAME = `e2e-min-revision-${Date.now()}`;
const DESC = "e2e minimum-revision floor skill";
const contentFor = (body: string) => `---\nname: ${NAME}\ndescription: ${DESC}\n---\n${body}`;

let artifactId = "";

test.afterAll(async () => {
  if (artifactId === "") {
    return;
  }
  const ctx = await playwrightRequest.newContext();
  try {
    const token = await tokenFor(ctx, "boss", "boss");
    await ctx.delete(`${API_BASE}/v1/admin/artifacts/${artifactId}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
  } finally {
    await ctx.dispose();
  }
});

test("the min-revision floor set in the browser is enforced by the real API, survives a reload, and clears the same way", async ({
  page,
  request,
}) => {
  // ── Seed two approved revisions through the API. The subject here is the
  // floor control, and driving create/submit/approve twice through the UI
  // would only re-test approval.spec.ts's revision-history flow at extra
  // wall-clock cost for no new evidence.
  const bossToken = await tokenFor(request, "boss", "boss");

  const createRes = await request.post(`${API_BASE}/v1/admin/artifacts`, {
    headers: { Authorization: `Bearer ${bossToken}` },
    data: { type: "skill", name: NAME, description: DESC, visibility: "org", content: contentFor("Revision one.") },
  });
  expect(createRes.ok(), `seed artifact: ${createRes.status()} ${await createRes.text()}`).toBeTruthy();
  artifactId = ((await createRes.json()) as { id: string }).id;

  const submit1 = await request.post(`${API_BASE}/v1/admin/artifacts/${artifactId}/submit`, {
    headers: { Authorization: `Bearer ${bossToken}` },
  });
  expect(submit1.ok(), `submit v1: ${submit1.status()} ${await submit1.text()}`).toBeTruthy();

  // boss2, because approving your own submission is refused server-side.
  const boss2Token = await tokenFor(request, "boss2", "boss2");

  async function rowVersion(token: string): Promise<number> {
    const res = await request.get(`${API_BASE}/v1/admin/artifacts/${artifactId}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(res.ok(), `get artifact: ${res.status()} ${await res.text()}`).toBeTruthy();
    return ((await res.json()) as { rowVersion: number }).rowVersion;
  }

  const approve1 = await request.post(`${API_BASE}/v1/admin/artifacts/${artifactId}/approve`, {
    headers: { Authorization: `Bearer ${boss2Token}`, "If-Match": `"${await rowVersion(boss2Token)}"` },
  });
  expect(approve1.ok(), `approve v1: ${approve1.status()} ${await approve1.text()}`).toBeTruthy();
  // Revision #1 exists and is current.

  const updateRes = await request.put(`${API_BASE}/v1/admin/artifacts/${artifactId}`, {
    headers: { Authorization: `Bearer ${bossToken}`, "If-Match": `"${await rowVersion(bossToken)}"` },
    data: { type: "skill", name: NAME, description: DESC, visibility: "org", content: contentFor("Revision two.") },
  });
  expect(updateRes.ok(), `edit to v2: ${updateRes.status()} ${await updateRes.text()}`).toBeTruthy();

  const submit2 = await request.post(`${API_BASE}/v1/admin/artifacts/${artifactId}/submit`, {
    headers: { Authorization: `Bearer ${bossToken}` },
  });
  expect(submit2.ok(), `submit v2: ${submit2.status()} ${await submit2.text()}`).toBeTruthy();

  const approve2 = await request.post(`${API_BASE}/v1/admin/artifacts/${artifactId}/approve`, {
    headers: { Authorization: `Bearer ${boss2Token}`, "If-Match": `"${await rowVersion(boss2Token)}"` },
  });
  expect(approve2.ok(), `approve v2: ${approve2.status()} ${await approve2.text()}`).toBeTruthy();
  // Revision #2 exists and is current; #1 still stands behind it.

  // ── Drive the control itself in the browser, as boss2 (an admin; setting
  // the floor carries no separation-of-duties rule, unlike approve).
  await login(page, "boss2", "boss2");
  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Artifacts" }).click();

  const row = page.locator("tr", { hasText: NAME });
  await expect(row).toBeVisible();
  // Baseline: no floor marker before the control is ever used.
  await expect(row.getByText(/^floor #/)).toHaveCount(0);

  await row.getByRole("button", { name: `History for ${NAME}`, exact: true }).click();
  // exact: true, because "#1" is also a substring of the non-current row's
  // own "Roll back to #1" button label, which would otherwise strict-mode
  // fail this lookup.
  await expect(page.getByText("#2", { exact: true })).toBeVisible();
  await expect(page.getByText("#1", { exact: true })).toBeVisible();

  // Pin the floor to revision #2, NOT #1. With exactly two revisions in this
  // fixture, #1 is the number a hardcoded constant would most plausibly
  // produce: the jsdom test for this control (ArtifactsPage.minRevision.test.tsx)
  // records that a first draft clicked row #1 and asserted minRevision:1, so a
  // component that always sent `minRevision: 1` regardless of which row was
  // clicked passed anyway. Pinning to #2 means a hardcoded-1 mutant, or any
  // send of a constant not equal to the clicked row's own number, is caught
  // by the minRevision === 2 check on the fresh read below.
  const revision2Row = page.locator("li").filter({ hasText: "#2" });
  page.once("dialog", (d) => {
    expect(d.message()).toBe(
      "Require revision #2 or newer for this artifact? Machines pinned below #2 will receive #2 on their next sync.",
    );
    void d.accept();
  });
  const setFloorResponse = page.waitForResponse(
    (r) => r.request().method() === "PUT" && r.url() === `${API_BASE}/v1/admin/artifacts/${artifactId}/min-revision`,
    { timeout: 15_000 },
  );
  await revision2Row.getByRole("button", { name: /require this or newer/i }).click();
  const setFloorRes = await setFloorResponse;
  expect(setFloorRes.status(), "the browser's min-revision PUT did not succeed").toBe(200);

  // Assert against a FRESH read, not the mutation's own optimistic cache
  // write. A full reload forces a new GET /v1/admin/artifacts, independent of
  // whatever React Query already holds in memory from the PUT's response.
  await page.reload();
  const rowAfterReload = page.locator("tr", { hasText: NAME });
  await expect(rowAfterReload.getByText("floor #2", { exact: true })).toBeVisible();

  // And independently of the browser entirely, as the server sees it: the
  // same "read back out of band" idiom identity-approval.spec.ts uses for its
  // own load-bearing assertion.
  const afterSet = await request.get(`${API_BASE}/v1/admin/artifacts/${artifactId}`, {
    headers: { Authorization: `Bearer ${boss2Token}` },
  });
  expect((await afterSet.json()) as { minRevision: number }).toMatchObject({ minRevision: 2 });

  // Clear it via the artifact TABLE ROW's own Clear button (point 7).
  // History does not need to be open for this one, unlike the removed panel
  // button. Unlike that removed button, this one confirms first: the
  // exact message ArtifactsPage.tsx sends, so a change to the wording is
  // caught here too, not only in the jsdom test.
  page.once("dialog", (d) => {
    expect(d.message()).toBe(
      `Clear the minimum-revision floor on ${NAME}? Machines pinned below the current revision are no longer held once they next sync.`,
    );
    void d.accept();
  });
  const clearFloorResponse = page.waitForResponse(
    (r) => r.request().method() === "PUT" && r.url() === `${API_BASE}/v1/admin/artifacts/${artifactId}/min-revision`,
    { timeout: 15_000 },
  );
  await rowAfterReload.getByRole("button", { name: `Clear floor for ${NAME}`, exact: true }).click();
  const clearFloorRes = await clearFloorResponse;
  expect(clearFloorRes.status(), "the browser's floor-clear PUT did not succeed").toBe(200);

  // Fresh read again, both ways: never the mutation's own optimistic
  // redraw.
  await page.reload();
  const rowAfterClear = page.locator("tr", { hasText: NAME });
  await expect(rowAfterClear.getByText(/^floor #/)).toHaveCount(0);
  await expect(rowAfterClear.getByRole("button", { name: `Clear floor for ${NAME}` })).toHaveCount(0);

  const afterClear = await request.get(`${API_BASE}/v1/admin/artifacts/${artifactId}`, {
    headers: { Authorization: `Bearer ${boss2Token}` },
  });
  expect((await afterClear.json()) as { minRevision: number }).toMatchObject({ minRevision: 0 });
});
