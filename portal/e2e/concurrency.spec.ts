import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

// Optimistic concurrency (docs/specs/2026-08-11-orbeat-optimistic-concurrency-design.md,
// docs/plans/orbeat-optimistic-concurrency-2026-08-11.md Task 12).
//
// §10.1 of the spec records why this file exists: from the moment `If-Match`
// enforcement landed on the server (Tasks 6-8) until the portal started
// sending it (Task 10), EVERY admin save returned 428 — and nothing in CI
// could see it, because every portal unit test mocks `fetch` (no CORS, no
// real server) and `portal/e2e/servers.spec.ts` had zero hits for PUT, Save
// or edit — it only ever *created* servers. That is the v1.16.0 defect
// verbatim: a real client/server seam broke, the mocked tests stayed green
// throughout, and only a browser talking to the real API could have noticed.
//
// This spec drives the browser against the REAL API for all three enforced
// mutations (server PUT, artifact PUT, artifact approve) so a CORS
// regression, a dropped `If-Match`, or a resurrected auto-retry in the
// conflict UI fails HERE, not silently.
//
// Per the pagination spec's note: there is no shared `./helpers` module in
// this suite — every file defines `login()` locally — so it is reused
// verbatim below, along with pagination.spec.ts's `adminToken()` (a
// direct-grant password token for seeding through the real API without
// driving the browser's OIDC redirect flow — the same mechanism
// scripts/smoke.sh uses).

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

/**
 * This spec's own Keycloak identity. Its four tests seed only a handful of
 * rows each, so it is not what loads the bucket, but it moves anyway: the
 * gate in internal/deploy derives its subject set from source (every spec
 * defining adminToken), and carving out the cheap specs would mean a
 * hand-maintained exemption list, which is the defect one level up.
 *
 * internal/ratelimit keys its token bucket on subject + azp, so every spec
 * that stays on `boss` shares one bucket with all the others. One constant
 * feeds both the browser login and the API token so the two can never drift
 * apart, which would silently put half this spec's traffic back on the
 * shared bucket.
 */
const E2E_USER = "e2e-concurrency";

/** Verbatim from pagination.spec.ts — see that file's doc comment for why. */
async function adminToken(request: APIRequestContext, user = E2E_USER, pass = E2E_USER): Promise<string> {
  const res = await request.post(KC_TOKEN_URL, {
    form: { grant_type: "password", client_id: "orbeat-cli", username: user, password: pass },
  });
  expect(res.ok(), `token request failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  const body = (await res.json()) as { access_token?: string };
  expect(body.access_token, "no access_token in token response").toBeTruthy();
  return body.access_token as string;
}

interface CreatedRow {
  id: string;
  rowVersion: number;
}

/** Seeds one server through the real API. */
async function createServer(
  request: APIRequestContext,
  token: string,
  name: string,
  status: "active" | "disabled" = "active",
): Promise<CreatedRow> {
  const res = await request.post(`${API_BASE}/v1/admin/servers`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      name,
      description: "concurrency e2e seed",
      transport: "http",
      endpointOrCommand: "https://example.invalid/mcp",
      version: "",
      protocolVersion: "",
      secretRef: "",
      status,
    },
  });
  expect(res.ok(), `create server ${name}: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()) as CreatedRow;
}

/** PUTs a full-replace server body with the given `If-Match`. Returns the raw response so callers assert status themselves. */
async function putServer(
  request: APIRequestContext,
  token: string,
  id: string,
  rowVersion: number,
  body: Record<string, unknown>,
) {
  return request.put(`${API_BASE}/v1/admin/servers/${id}`, {
    headers: { Authorization: `Bearer ${token}`, "If-Match": `"${rowVersion}"` },
    data: body,
  });
}

/** Seeds one skill artifact through the real API (draft state). */
async function createArtifact(request: APIRequestContext, token: string, name: string, content: string): Promise<CreatedRow> {
  const res = await request.post(`${API_BASE}/v1/admin/artifacts`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      type: "skill",
      name,
      description: "concurrency e2e seed",
      content,
      memoryScope: "",
      memorySeed: "",
      version: "",
      visibility: "org",
    },
  });
  expect(res.ok(), `create artifact ${name}: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()) as CreatedRow;
}

/** PUTs a full-replace artifact body with the given `If-Match`. Returns the raw response so callers assert status themselves. */
async function putArtifact(
  request: APIRequestContext,
  token: string,
  id: string,
  rowVersion: number,
  body: Record<string, unknown>,
) {
  return request.put(`${API_BASE}/v1/admin/artifacts/${id}`, {
    headers: { Authorization: `Bearer ${token}`, "If-Match": `"${rowVersion}"` },
    data: body,
  });
}

const CONFLICT_TEXT = /this changed since you loaded it — reload to see the current state/i;

const RUN = `cc${Date.now()}`;

test.describe("optimistic concurrency (real API + real browser)", () => {
  // Registered immediately after a create call succeeds, never deferred to
  // the end of the test — an in-test try/finally does NOT run on a
  // Playwright test timeout (the timeout abandons the test function
  // mid-await, so a wrapping finally never resumes). Proven live during
  // v1.22.0: 105 seeded pagination rows leaked under the old try/finally
  // shape. afterEach is a separate lifecycle step Playwright always runs,
  // pass/fail/timeout alike.
  let seededServerIds: string[] = [];
  let seededArtifactIds: string[] = [];

  test.afterEach(async ({ request }) => {
    const token = await adminToken(request);
    for (const id of seededServerIds) {
      await request.delete(`${API_BASE}/v1/admin/servers/${id}`, { headers: { Authorization: `Bearer ${token}` } });
    }
    for (const id of seededArtifactIds) {
      await request.delete(`${API_BASE}/v1/admin/artifacts/${id}`, { headers: { Authorization: `Bearer ${token}` } });
    }
    seededServerIds = [];
    seededArtifactIds = [];
  });

  // Test 1: the gap named in spec §10.1 directly. servers.spec.ts only ever
  // creates servers — no e2e anywhere drove a server edit+save before this,
  // so the CORS/If-Match break (428 on every save) was caught for nothing
  // and would have shipped invisibly. This drives the real browser fetch
  // (via page.waitForResponse, not a test-issued API call) through Save so a
  // CORS regression (e.g. reverting `If-Match` out of
  // Access-Control-Allow-Headers) fails HERE.
  test("editing and saving a server through the browser reaches the real API", async ({ page, request }) => {
    const token = await adminToken(request);
    const name = `${RUN}-srv-edit`;
    const created = await createServer(request, token, name, "active");
    seededServerIds.push(created.id);

    await login(page, E2E_USER, E2E_USER);
    await gotoAdmin(page, "Servers");

    const row = page.locator("tr", { hasText: name });
    await expect(row).toBeVisible();
    await expect(row.getByText("active", { exact: true })).toBeVisible();

    // Registered before the click that triggers it: waits for the BROWSER's
    // own PUT (not a request this test issues itself) so a CORS block (the
    // preflight is refused, the actual PUT is never sent) shows up as this
    // promise timing out / the response never resolving, not as a false
    // green from an out-of-band API call.
    const putResponse = page.waitForResponse(
      (r) => r.request().method() === "PUT" && r.url() === `${API_BASE}/v1/admin/servers/${created.id}`,
      { timeout: 15_000 },
    );

    await page.getByRole("button", { name: `Edit ${name}`, exact: true }).click();
    const form = page.locator("form");
    await form.getByLabel("Status").selectOption("disabled");
    await form.getByRole("button", { name: /^save$/i }).click();

    const putRes = await putResponse;
    expect(putRes.status(), "the browser's PUT did not succeed — CORS/If-Match round-trip broken").toBe(200);

    await expect(page.locator("form")).toHaveCount(0, { timeout: 15_000 });
    await expect(row.getByText("disabled", { exact: true })).toBeVisible();

    // Persisted server-side (not merely reflected by an optimistic client
    // update) — read back through the API, independent of the browser.
    const getRes = await request.get(`${API_BASE}/v1/admin/servers/${created.id}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(getRes.ok()).toBeTruthy();
    const after = (await getRes.json()) as { status: string };
    expect(after.status).toBe("disabled");
  });

  // Test 2: a real 412 on a server edit, and the Task-11 regression path —
  // the Reload button once defaulted to type="submit" inside the same
  // <form>, so clicking it also fired onSubmit and resubmitted the stale
  // PUT a second time.
  test("a stale server save is rejected with a conflict notice, never overwrites the underneath change, and Reload does not resubmit", async ({
    page,
    request,
  }) => {
    const token = await adminToken(request);
    const name = `${RUN}-srv-conflict`;
    const created = await createServer(request, token, name, "active");
    seededServerIds.push(created.id);

    await login(page, E2E_USER, E2E_USER);
    await gotoAdmin(page, "Servers");

    const row = page.locator("tr", { hasText: name });
    await expect(row).toBeVisible();
    await expect(row.getByText("active", { exact: true })).toBeVisible();

    // Opens the edit form — ServersPage snapshots the row (and its
    // rowVersion) the list held at THIS moment into React state; that
    // snapshot does not track later list refetches, which is what makes it
    // possible to go stale underneath an already-open form.
    await page.getByRole("button", { name: `Edit ${name}`, exact: true }).click();
    const form = page.locator("form");
    await expect(form.getByLabel("Status")).toHaveValue("active");

    // A second admin's write the browser never saw, via the real API.
    const underneath = await putServer(request, token, created.id, created.rowVersion, {
      name,
      description: "changed by a second admin",
      transport: "http",
      endpointOrCommand: "https://example.invalid/mcp",
      version: "",
      protocolVersion: "",
      secretRef: "",
      status: "disabled",
    });
    expect(underneath.ok(), `underneath server PUT: ${underneath.status()} ${await underneath.text()}`).toBeTruthy();

    // Tracked from here on: the stale save below must reach the API exactly
    // once, and Reload must never re-issue it.
    const putUrls: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "PUT" && req.url() === `${API_BASE}/v1/admin/servers/${created.id}`) {
        putUrls.push(req.url());
      }
    });

    // The browser's stale form flips status back to "active" — if the
    // precondition were bypassed (last-write-wins), this would clobber the
    // underneath admin's "disabled" write.
    await form.getByLabel("Status").selectOption("active");
    await form.getByRole("button", { name: /^save$/i }).click();

    await expect(page.getByText(CONFLICT_TEXT)).toBeVisible();
    expect(putUrls.length, "the stale save should have reached the API exactly once").toBe(1);

    // The underneath admin's write survived — checked via the API, not the
    // (deliberately un-refetched) UI.
    const afterFailedSave = await request.get(`${API_BASE}/v1/admin/servers/${created.id}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(afterFailedSave.ok()).toBeTruthy();
    const afterBody = (await afterFailedSave.json()) as { status: string };
    expect(afterBody.status, "the stale save overwrote the underneath admin's change").toBe("disabled");

    await page.getByRole("button", { name: /reload/i }).click();

    // Reload discards the stale edit and closes the form, then refetches
    // the list — the row now shows the CURRENT (underneath) value.
    await expect(page.locator("form")).toHaveCount(0, { timeout: 15_000 });
    await expect(row.getByText("disabled", { exact: true })).toBeVisible();

    // Reload never re-issued the write (the Task 11 regression path).
    expect(putUrls.length).toBe(1);
  });

  // Test 3: same guarantees for an artifact edit. ArtifactEditForm behaves
  // differently from the server form — it holds no list-row snapshot and
  // instead does a fresh by-id GET every time it opens (useAdminArtifact,
  // gcTime: 0) — so "brings the current values" is proven by reopening Edit
  // after Reload and observing a fresh fetch, not just by a list refetch.
  test("a stale artifact save is rejected with a conflict notice, never overwrites the underneath change, and Reload does not resubmit", async ({
    page,
    request,
  }) => {
    const token = await adminToken(request);
    const name = `${RUN}-artifact-conflict`;
    const original = `---\nname: ${name}\ndescription: e2e concurrency seed\n---\noriginal content\n`;
    const created = await createArtifact(request, token, name, original);
    seededArtifactIds.push(created.id);

    await login(page, E2E_USER, E2E_USER);
    await gotoAdmin(page, "Artifacts");

    await page.getByRole("button", { name: `Edit ${name}`, exact: true }).click();
    const form = page.locator("form");
    // Waits for the by-id GET (whose rowVersion the Save below will echo
    // back as If-Match) to resolve before the underneath change runs.
    await expect(form.getByLabel(/content/i)).toHaveValue(/original content/);

    const changedUnderneath = `---\nname: ${name}\ndescription: e2e concurrency seed\n---\nchanged underneath\n`;
    const underneath = await putArtifact(request, token, created.id, created.rowVersion, {
      type: "skill",
      name,
      description: "changed by a second admin",
      content: changedUnderneath,
      memoryScope: "",
      memorySeed: "",
      version: "",
      visibility: "org",
    });
    expect(underneath.ok(), `underneath artifact PUT: ${underneath.status()} ${await underneath.text()}`).toBeTruthy();

    const putUrls: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "PUT" && req.url() === `${API_BASE}/v1/admin/artifacts/${created.id}`) {
        putUrls.push(req.url());
      }
    });

    const staleAttempt = `---\nname: ${name}\ndescription: e2e concurrency seed\n---\nchanged in the stale browser form\n`;
    await form.getByLabel(/content/i).fill(staleAttempt);
    await form.getByRole("button", { name: /^save$/i }).click();

    await expect(page.getByText(CONFLICT_TEXT)).toBeVisible();
    expect(putUrls.length, "the stale save should have reached the API exactly once").toBe(1);

    const afterFailedSave = await request.get(`${API_BASE}/v1/admin/artifacts/${created.id}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(afterFailedSave.ok()).toBeTruthy();
    const afterBody = (await afterFailedSave.json()) as { content: string };
    expect(afterBody.content, "the stale save overwrote the underneath admin's change").toContain("changed underneath");
    expect(afterBody.content).not.toContain("changed in the stale browser form");

    await page.getByRole("button", { name: /reload/i }).click();
    await expect(page.locator("form")).toHaveCount(0, { timeout: 15_000 });

    // Reload never re-issued the write.
    expect(putUrls.length).toBe(1);

    // "Brings the current values": reopening Edit does a FRESH by-id GET
    // (gcTime: 0 — see queries.ts's useAdminArtifact doc comment) and shows
    // the underneath admin's content, not the discarded stale edit.
    await page.getByRole("button", { name: `Edit ${name}`, exact: true }).click();
    await expect(page.locator("form").getByLabel(/content/i)).toHaveValue(/changed underneath/);
  });

  // Test 4: the governance hazard the whole slice exists for (see
  // ConflictNotice.tsx's doc comment and admin_artifact_review.go's
  // handleApproveArtifact comment, which names this EXACT scenario: "a
  // reviewer reads pending content, a second admin PUTs a replacement...
  // and the reviewer's stale Approve click would otherwise freeze content
  // they never saw into the append-only revision chain and publish it to
  // both channels". Asserting only that the UI shows a conflict message
  // would be asserting the wrong thing — the assertion that matters is that
  // nothing was actually published, checked via the API.
  //
  // Two distinct admins (this spec's own identity authors/edits, boss2
  // reviews), matching approval.spec.ts's established pattern: approving
  // your own submission is rejected server-side for an unrelated reason
  // (separation of duties), and keeping the two concerns apart makes this
  // test unambiguously about the version check.
  test("a stale approve is rejected with a conflict notice, and nothing is published", async ({ page, request }) => {
    const authorToken = await adminToken(request, E2E_USER, E2E_USER);
    const name = `${RUN}-artifact-approve`;
    const v1 = `---\nname: ${name}\ndescription: e2e stale-approve seed\n---\nv1 content\n`;
    const created = await createArtifact(request, authorToken, name, v1);
    seededArtifactIds.push(created.id);

    const submitRes = await request.post(`${API_BASE}/v1/admin/artifacts/${created.id}/submit`, {
      headers: { Authorization: `Bearer ${authorToken}` },
    });
    expect(submitRes.ok(), `submit: ${submitRes.status()} ${await submitRes.text()}`).toBeTruthy();

    await login(page, "boss2", "boss2");
    await gotoAdmin(page, "Review queue");

    const card = page.locator(".rounded-xl", { hasText: name });
    await expect(card).toBeVisible();
    // The card must be showing v1 (the content whose rowVersion its Approve
    // button will echo back as If-Match) before the underneath change runs.
    await expect(card.getByText(/v1 content/)).toBeVisible();

    // A second admin edits the pending artifact underneath the reviewer —
    // this resets it to draft (store.UpdateArtifact) — then resubmits so it
    // is pending again, at a NEWER row_version than the open card holds.
    const current = await request.get(`${API_BASE}/v1/admin/artifacts/${created.id}`, {
      headers: { Authorization: `Bearer ${authorToken}` },
    });
    expect(current.ok()).toBeTruthy();
    const currentRowVersion = ((await current.json()) as { rowVersion: number }).rowVersion;

    const v2 = `---\nname: ${name}\ndescription: e2e stale-approve seed\n---\nv2 content, never reviewed\n`;
    const editRes = await putArtifact(request, authorToken, created.id, currentRowVersion, {
      type: "skill",
      name,
      description: "edited underneath the reviewer",
      content: v2,
      memoryScope: "",
      memorySeed: "",
      version: "",
      visibility: "org",
    });
    expect(editRes.ok(), `underneath edit: ${editRes.status()} ${await editRes.text()}`).toBeTruthy();

    const resubmitRes = await request.post(`${API_BASE}/v1/admin/artifacts/${created.id}/submit`, {
      headers: { Authorization: `Bearer ${authorToken}` },
    });
    expect(resubmitRes.ok(), `resubmit: ${resubmitRes.status()} ${await resubmitRes.text()}`).toBeTruthy();

    await card.getByRole("button", { name: /^approve$/i }).click();

    await expect(card.getByText(CONFLICT_TEXT)).toBeVisible();

    // Nothing was PUBLISHED: no approved snapshot, no revision. This is the
    // governance-critical check — not the UI message above.
    const afterStaleApprove = await request.get(`${API_BASE}/v1/admin/artifacts/${created.id}`, {
      headers: { Authorization: `Bearer ${authorToken}` },
    });
    expect(afterStaleApprove.ok()).toBeTruthy();
    const afterBody = (await afterStaleApprove.json()) as { approved: boolean; approvedContent?: string };
    expect(afterBody.approved, "a stale approve published a snapshot").toBe(false);
    expect(afterBody.approvedContent, "a stale approve published content the reviewer never saw").toBeFalsy();

    const revisionsRes = await request.get(`${API_BASE}/v1/admin/artifacts/${created.id}/revisions`, {
      headers: { Authorization: `Bearer ${authorToken}` },
    });
    expect(revisionsRes.ok()).toBeTruthy();
    const revisionsBody = (await revisionsRes.json()) as { revisions: unknown[] };
    expect(revisionsBody.revisions, "a stale approve appended a revision").toHaveLength(0);
  });
});
