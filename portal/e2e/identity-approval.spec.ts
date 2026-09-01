import { expect, request as playwrightRequest, test, type APIRequestContext, type Page } from "@playwright/test";

// Artifact identity through approval
// (docs/specs/2026-08-22-orbeat-artifact-identity-approval-design.md, Task 8).
//
// An admin renames an approved artifact. The table shows the new name, and every
// entitled developer keeps receiving the OLD one until a second admin approves
// the change. Three things have to be true at once for that to be usable, and
// only a real browser against the real API can show all three together:
//
//  1. the portal SENDS the rename (a PUT with If-Match, through CORS) and gets
//     a 200, where it used to get a 400 refusal;
//  2. the portal SHOWS the gap, on the artifacts list and in the review queue,
//     driven by approvedType/approvedName/approvedVisibility as the API really
//     serves them, not as a jsdom fixture hands them to a component;
//  3. distribution does not move until approval, read back out of
//     /v1/sync/artifacts as a normal entitled user.
//
// Point 2 is why this file exists rather than another unit test. The list
// indicator depends on the three approved_* columns riding the SLIM list
// projection: the artifacts table is fetched WITHOUT ?include=content, so if
// those columns were ever dropped from artifactSlimCols the badge would vanish
// in production while every jsdom test that builds its own props stayed green.
// That is the v1.16.0 defect shape, and this repo has shipped it twice.
//
// scripts/smoke.sh carries the other half of the proof, the half a browser
// cannot reach: it drives the real orbeat-sync binary and asserts the file on
// disk keeps its old path until approval and MOVES after it.

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

async function gotoAdmin(page: Page, section: string) {
  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: section }).click();
}

const API_BASE = "http://localhost:8080";
const KC_TOKEN_URL = "http://localhost:8088/realms/orbeat/protocol/openid-connect/token";

/** Direct-grant token, the same mechanism scripts/smoke.sh uses. */
async function tokenFor(request: APIRequestContext, user: string, pass: string): Promise<string> {
  const res = await request.post(KC_TOKEN_URL, {
    form: { grant_type: "password", client_id: "orbeat-cli", username: user, password: pass },
  });
  expect(res.ok(), `token request for ${user} failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  const body = (await res.json()) as { access_token?: string };
  expect(body.access_token, `no access_token for ${user}`).toBeTruthy();
  return body.access_token as string;
}

/**
 * The DB id of the `orbeat-user` role, creating the row if no spec has yet.
 *
 * orbeat roles live in Postgres and are NOT auto-created on login: the resolver
 * maps a token's realm-role names to existing rows and silently drops the rest
 * (internal/authz/resolver.go). Nothing seeds them, so on a fresh stack the
 * roles table is empty and alice resolves to no role at all, which would make
 * every /v1/sync/artifacts read below return an empty list and every assertion
 * on it vacuous.
 *
 * "orbeat-user" specifically, and not a per-run name: it has to be a role the
 * realm actually puts in alice's token, or the entitlement grants her nothing.
 *
 * Idempotent, and deliberately tolerant of a concurrent 409. portal.spec.ts
 * creates the same role through the UI and spec files run in parallel across
 * workers, so either side can get there first. Its own assertion is that the
 * role appears in the list, which stays true when its create loses the race;
 * scripts/smoke.sh accepts 201|409 here for the same reason.
 */
async function ensureUserRole(request: APIRequestContext, token: string): Promise<string> {
  const auth = { Authorization: `Bearer ${token}` };
  const find = async () => {
    const res = await request.get(`${API_BASE}/v1/admin/roles?limit=500`, { headers: auth });
    expect(res.ok(), `list roles: ${res.status()} ${await res.text()}`).toBeTruthy();
    return ((await res.json()) as { roles: { id: string; name: string }[] }).roles.find(
      (r) => r.name === "orbeat-user",
    );
  };
  const existing = await find();
  if (existing) {
    return existing.id;
  }
  const created = await request.post(`${API_BASE}/v1/admin/roles`, {
    headers: auth,
    data: { name: "orbeat-user" },
  });
  expect(
    created.ok() || created.status() === 409,
    `create orbeat-user: ${created.status()} ${await created.text()}`,
  ).toBeTruthy();
  const after = await find();
  expect(after, "orbeat-user is still missing after creating it").toBeTruthy();
  return after!.id;
}

interface SyncArtifact {
  type: string;
  name: string;
  content: string;
}

/**
 * What a normal entitled user is being served right now, straight off the
 * Channel-2 route the sync client calls. Minted fresh per call: Keycloak's
 * default access-token lifetime is 5 minutes and a browser flow in between can
 * eat a good part of it.
 */
async function distributedToAlice(request: APIRequestContext): Promise<SyncArtifact[]> {
  const token = await tokenFor(request, "alice", "alice");
  const res = await request.get(`${API_BASE}/v1/sync/artifacts`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(res.ok(), `/v1/sync/artifacts failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return ((await res.json()) as { artifacts: SyncArtifact[] }).artifacts;
}

// Per-run unique and all-digit-suffixed so it satisfies the artifact slugRe,
// the servers.spec.ts lesson: a fixed name lets a second run against a
// surviving stack pass vacuously on the previous run's rows, or 409 on create.
//
// The "e2e-identity" prefix is load-bearing beyond uniqueness. Spec files run in
// parallel across workers and the admin list is keyset-ordered (type, name, id)
// with a 100-row default page, so a name must sort inside the first page while
// pagination.spec.ts has 105 "pg<ts>" rows seeded. "e2e-" sorts before "pg", and
// type "skill" sorts before "subagent", so this row is on page 1 either way.
const NAME = `e2e-identity-${Date.now()}`;
const RENAMED = `${NAME}-renamed`;
const DESC = "e2e identity-through-approval skill";
const BODY_V1 = "Distributed body, approved before the rename.";
const BODY_V2 = "Rewritten body, saved together with the rename.";

const contentFor = (name: string, body: string) => `---\nname: ${name}\ndescription: ${DESC}\n---\n${body}`;

let artifactId = "";

// boss authors and boss2 approves, so the two tests need separate browser
// contexts, which is what a fresh Playwright test gives. Serial because the
// second test resumes exactly where the first left the artifact.
test.describe.configure({ mode: "serial" });

test.afterAll(async () => {
  if (artifactId === "") {
    return;
  }
  // A dedicated context rather than the `request` fixture: fixtures are
  // test-scoped, and this has to run once, after the last test, whatever its
  // outcome. Deleting the artifact cascades its role entitlement away.
  //
  // Not optional hygiene. A surviving role-visibility, file-backed artifact
  // entitled to orbeat-user changes what /v1/sync/artifacts serves alice, and
  // scripts/smoke.sh's F4 scenario asserts .artifacts.updated == 1 on exactly
  // that fixture. Leaving this row behind would arm a failure in another suite.
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

test("boss renames an approved artifact in the browser; developers keep receiving the old name", async ({
  page,
  request,
}) => {
  // ── Seed through the API, not the browser. The subject here is the rename,
  // and driving create/entitle/submit/approve through the UI would only re-test
  // approval.spec.ts's flow at four times the wall-clock cost.
  const bossToken = await tokenFor(request, "boss", "boss");

  const roleId = await ensureUserRole(request, bossToken);

  const createRes = await request.post(`${API_BASE}/v1/admin/artifacts`, {
    headers: { Authorization: `Bearer ${bossToken}` },
    data: {
      type: "skill",
      name: NAME,
      description: DESC,
      visibility: "role",
      content: contentFor(NAME, BODY_V1),
    },
  });
  expect(createRes.ok(), `seed artifact: ${createRes.status()} ${await createRes.text()}`).toBeTruthy();
  artifactId = ((await createRes.json()) as { id: string }).id;

  const entRes = await request.post(`${API_BASE}/v1/admin/artifact-entitlements`, {
    headers: { Authorization: `Bearer ${bossToken}` },
    data: { roleId, artifactId },
  });
  expect(entRes.ok(), `entitle orbeat-user: ${entRes.status()} ${await entRes.text()}`).toBeTruthy();

  const submitRes = await request.post(`${API_BASE}/v1/admin/artifacts/${artifactId}/submit`, {
    headers: { Authorization: `Bearer ${bossToken}` },
  });
  expect(submitRes.ok(), `seed submit: ${submitRes.status()} ${await submitRes.text()}`).toBeTruthy();

  // boss2, because approving your own submission is refused server-side.
  const boss2Token = await tokenFor(request, "boss2", "boss2");
  const beforeApprove = await request.get(`${API_BASE}/v1/admin/artifacts/${artifactId}`, {
    headers: { Authorization: `Bearer ${boss2Token}` },
  });
  const rowVersion = ((await beforeApprove.json()) as { rowVersion: number }).rowVersion;
  const approveRes = await request.post(`${API_BASE}/v1/admin/artifacts/${artifactId}/approve`, {
    headers: { Authorization: `Bearer ${boss2Token}`, "If-Match": `"${rowVersion}"` },
  });
  expect(approveRes.ok(), `seed approve: ${approveRes.status()} ${await approveRes.text()}`).toBeTruthy();

  // The baseline every assertion below is measured against.
  const distributedBefore = await distributedToAlice(request);
  expect(
    distributedBefore.find((a) => a.name === NAME)?.content,
    "the seeded artifact never reached distribution, so nothing below would be evidence",
  ).toContain(BODY_V1);

  // ── The rename, in the browser.
  await login(page, "boss", "boss");
  await gotoAdmin(page, "Artifacts");

  const row = page.locator("tr", { hasText: NAME });
  await expect(row).toBeVisible();
  await expect(row.getByText("approved", { exact: true })).toBeVisible();
  // Nothing to report before the edit. Anchored on the phrase alone, so it
  // would also catch a marker announcing a gap that does not exist.
  await expect(row.getByText(/distributing as/)).toHaveCount(0);

  // Registered before the click that triggers it, so a CORS regression (the
  // preflight is refused and the PUT is never sent) surfaces as this promise
  // timing out rather than as a false green from a test-issued API call.
  const putResponse = page.waitForResponse(
    (r) => r.request().method() === "PUT" && r.url() === `${API_BASE}/v1/admin/artifacts/${artifactId}`,
    { timeout: 15_000 },
  );

  await page.getByRole("button", { name: `Edit ${NAME}`, exact: true }).click();
  // Field lookups scoped to the open <form>: row-action buttons carry aria-labels
  // like "Edit <name>" and getByLabel matches case-insensitive substrings.
  const form = page.locator("form");
  await form.getByLabel("Name", { exact: true }).fill(RENAMED);
  await form.getByLabel("Content", { exact: true }).fill(contentFor(RENAMED, BODY_V2));
  await form.getByRole("button", { name: /^save$/i }).click();

  const putRes = await putResponse;
  expect(
    putRes.status(),
    "the browser's rename PUT did not succeed; the identity lock or the If-Match round-trip is broken",
  ).toBe(200);
  await expect(page.locator("form")).toHaveCount(0, { timeout: 15_000 });

  // ── The gap is on screen, in the list, without ?include=content.
  const renamedRow = page.locator("tr", { hasText: RENAMED });
  await expect(renamedRow.getByText("draft", { exact: true })).toBeVisible();
  await expect(renamedRow.getByText(`distributing as ${NAME}`, { exact: true })).toBeVisible();

  // ── THE LOAD-BEARING ASSERTION. Read back as a normal entitled user, out of
  // band from the browser. Both directions: the old pair still served, the new
  // pair nowhere. "the old name is present" alone passes on a payload carrying
  // BOTH, which is a real failure mode (a duplicate rather than a move).
  const distributedAfterRename = await distributedToAlice(request);
  const stillOld = distributedAfterRename.find((a) => a.name === NAME);
  expect(stillOld, `an UNAPPROVED rename removed ${NAME} from distribution`).toBeTruthy();
  expect(stillOld!.content, "the unapproved body reached distribution").toContain(BODY_V1);
  expect(stillOld!.content).not.toContain(BODY_V2);
  expect(
    distributedAfterRename.filter((a) => a.name === RENAMED),
    `the new name reached distribution before any reviewer approved it`,
  ).toHaveLength(0);

  // ── And on the edit form, a separate surface with its own wording, which the
  // admin who typed the rename is the one most likely to be looking at. The
  // form has no Cancel control (it closes on a successful save), so it is left
  // open below the table; the row's own actions stay clickable regardless.
  await page.getByRole("button", { name: `Edit ${RENAMED}`, exact: true }).click();
  await expect(
    page.locator("form").getByText(`Developers still receive name ${NAME} until the saved changes are distributed.`),
  ).toBeVisible();

  await renamedRow.getByRole("button", { name: `Submit ${RENAMED}`, exact: true }).click();
  await expect(renamedRow.getByText("pending", { exact: true })).toBeVisible();
});

test("boss2 sees the identity diff in the review queue, and approving it moves distribution", async ({
  page,
  request,
}) => {
  await login(page, "boss2", "boss2");
  await gotoAdmin(page, "Review queue");
  await expect(page.getByRole("heading", { name: /review queue/i })).toBeVisible();

  const card = page.locator(".rounded-xl", { hasText: RENAMED });
  await expect(card).toBeVisible();
  // The reviewer is approving a path change on every machine that receives this
  // artifact. Assert the arrow line, not just the heading: a heading with an
  // empty list underneath would tell the reviewer nothing.
  await expect(card.getByText("Approving this also changes what every machine receives:")).toBeVisible();
  await expect(card.getByText(new RegExp(`${NAME}\\s*→\\s*${RENAMED}`))).toBeVisible();

  await card.getByRole("button", { name: /^approve$/i }).click();
  await expect(page.locator(".rounded-xl", { hasText: RENAMED })).toHaveCount(0);

  // The marker is gone once there is no gap left to report.
  await page.getByRole("link", { name: "Artifacts" }).click();
  const row = page.locator("tr", { hasText: RENAMED });
  await expect(row.getByText("approved", { exact: true })).toBeVisible();
  await expect(row.getByText(/distributing as/)).toHaveCount(0);

  // The pair flips together: new name, new body, and the old name gone rather
  // than left behind beside it.
  const distributed = await distributedToAlice(request);
  const nowNew = distributed.find((a) => a.name === RENAMED);
  expect(nowNew, `the approved rename never reached distribution`).toBeTruthy();
  expect(nowNew!.content, "the new name is distributed without the newly approved body").toContain(BODY_V2);
  expect(
    distributed.filter((a) => a.name === NAME),
    "the old name is still being distributed after approval",
  ).toHaveLength(0);
});
