import { expect, test, type APIRequestContext, type Locator, type Page } from "@playwright/test";

// Portal unit tests mock `fetch` and were all green while the admin console
// was truncated at 100 rows, the approver's diff panel was blank, and the
// edit form was primed to wipe seed memory (v1.14.0/v1.16.0 failure shape).
// This spec drives the REAL API through the REAL browser — the only gate in
// this repo that can catch that class of defect for pagination.
//
// Two things the plan's Task 12 sketch got wrong, verified while writing this:
//   1. It assumed a portal page size of 50. Task 10 sends no `?limit` at all,
//      so the server's own default (internal/api/paging.go's
//      defaultListLimit = 100) applies. Seeding is sized off 100, not 50.
//   2. It imported `{ adminToken, loginAsAdmin, apiBase }` from `./helpers`.
//      No such module exists in this repo — every spec here (servers,
//      dropdowns, approval, portal, expired-session) defines `login()`
//      locally. Reused verbatim below rather than reinvented.
//
// What this spec does NOT prove (review C7 rule 4): Load-more is browser-
// verified here only for Servers and the Review queue. Roles, Entitlements,
// Artifact-entitlements, the plain Artifacts list, and per-artifact Revisions
// all share the same `useAdminList` infinite-query plumbing (queries.ts) but
// have zero browser coverage of their own Load-more control in this file —
// their correctness rests entirely on the shared implementation exercised
// here, not on independent proof. The `limit` field the pagination envelope
// carries (`Page<K,Row>.limit`, added in Task 10) is also never asserted by
// any test below.

/** Verbatim from servers.spec.ts / dropdowns.spec.ts / approval.spec.ts / portal.spec.ts. */
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
 * This spec's own Keycloak identity, and the one that matters most: its two
 * 105-row walks put about 525 direct API calls on a single bucket (server
 * create and delete, artifact create, submit and delete), the largest single
 * contributor in deploy/docker-compose.yml's ORBEAT_RATELIMIT_BURST
 * derivation.
 *
 * internal/ratelimit keys its token bucket on subject + azp, so every spec
 * that stays on `boss` shares one bucket with all the others. One constant
 * feeds both the browser login and the API token so the two can never drift
 * apart, which would silently put half this spec's traffic back on the
 * shared bucket.
 */
const E2E_USER = "e2e-pagination";

/**
 * Direct-grant (password grant) admin token for API seeding, bypassing the
 * browser entirely. This is NOT a reimplementation of the portal's
 * PKCE/redirect SSO flow that login() drives above — it's the same mechanism
 * scripts/smoke.sh already uses to seed data through the real API
 * (grant_type=password against `orbeat-cli`, a public client with
 * directAccessGrantsEnabled=true in the dev realm — see
 * deploy/keycloak/orbeat-realm.json). Seeding 100+ rows one field at a time
 * through the browser form would make this spec unusably slow; every other
 * spec in this suite seeds only a handful of rows and never needed this.
 */
async function adminToken(request: APIRequestContext, user = E2E_USER, pass = E2E_USER): Promise<string> {
  const res = await request.post(KC_TOKEN_URL, {
    form: { grant_type: "password", client_id: "orbeat-cli", username: user, password: pass },
  });
  expect(res.ok(), `token request failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  const body = (await res.json()) as { access_token?: string };
  expect(body.access_token, "no access_token in token response").toBeTruthy();
  return body.access_token as string;
}

/**
 * Bounded-concurrency map. Seeding 100+ rows strictly sequentially is
 * needlessly slow; firing all of them at once via unbounded Promise.all risks
 * exhausting the DB connection pool (no explicit ORBEAT_DB_MAX_CONNS is set —
 * see internal/store/store.go — so pgxpool's default applies). A small fixed
 * worker count gets most of the speedup without that risk.
 */
async function mapConcurrent<T, R>(
  items: T[],
  concurrency: number,
  fn: (item: T, index: number) => Promise<R>,
): Promise<R[]> {
  const results: R[] = new Array(items.length);
  let next = 0;
  async function worker() {
    for (let i = next++; i < items.length; i = next++) {
      results[i] = await fn(items[i], i);
    }
  }
  await Promise.all(Array.from({ length: Math.min(concurrency, items.length) }, () => worker()));
  return results;
}

/**
 * Deletes every id via DELETE {base}/{id}, concurrently. NOT best-effort: each
 * delete's status is asserted. Silently ignoring a failed delete (the
 * original shape) would leave the suite green while leaking rows into the
 * next run's counts — and with a rate limiter now live (rate-limiting plan
 * Task 8, C4/C5), a 429 mid-teardown is exactly the failure mode that would
 * do it: 212 seeded rows across one test, all torn down against the SAME
 * bucket the seeding itself just drained, `retries: 1` in CI would then
 * launder an intermittent trip into a flake instead of a visible failure.
 */
async function deleteAll(request: APIRequestContext, token: string, base: string, ids: string[]): Promise<void> {
  if (ids.length === 0) return;
  await mapConcurrent(ids, 6, async (id) => {
    const res = await request.delete(`${API_BASE}${base}/${id}`, { headers: { Authorization: `Bearer ${token}` } });
    expect(res.ok(), `delete ${base}/${id}: ${res.status()} ${await res.text()}`).toBeTruthy();
  });
}

/**
 * Walks GET {base} by cursor, calling `onPage` with each page's raw response
 * text, until `onPage` reports the target found or the list is exhausted
 * (empty `nextCursor`). Bounded by the list's own exhaustion, not a clock.
 *
 * GET /v1/admin/artifacts has no filter narrow enough to scope an existence
 * check to one seeded row (only `?state`, and freshly-seeded rows across
 * this whole suite are almost all "draft"), so unlike audit-filters.spec.ts's
 * `?action=` fix this cannot narrow the competing volume, it can only stop
 * assuming the row landed on page 1. Reproduced live: seeding 105 rows of the
 * same artifact `type` ahead of this test's row in (type, name) sort order
 * pushed it off an unfiltered page-1 response entirely.
 */
async function walkArtifactPages(
  request: APIRequestContext,
  token: string,
  onPage: (pageText: string) => boolean,
): Promise<boolean> {
  let cursor = "";
  for (let i = 0; i < 50; i++) {
    const url = cursor
      ? `${API_BASE}/v1/admin/artifacts?cursor=${encodeURIComponent(cursor)}`
      : `${API_BASE}/v1/admin/artifacts`;
    const res = await request.get(url, { headers: { Authorization: `Bearer ${token}` } });
    expect(res.ok(), `list artifacts: ${res.status()} ${await res.text()}`).toBeTruthy();
    const text = await res.text();
    if (onPage(text)) return true;
    const parsed = JSON.parse(text) as { nextCursor?: string };
    if (!parsed.nextCursor) return false;
    cursor = parsed.nextCursor;
  }
  return false;
}

/**
 * Clicks "Load more" on the currently open admin list, bounded by the
 * control's own exhaustion (it only renders while `hasNextPage` is true --
 * ArtifactsPage.tsx), until `target` is visible or the list runs out. The
 * post-click wait is the same "poll a real row-count growth" technique this
 * file's first test ("Load more appends pages of servers") already uses for
 * the identical class of problem, not a fixed sleep.
 */
async function revealRow(page: Page, target: Locator): Promise<void> {
  const loadMore = page.getByRole("button", { name: "Load more", exact: true });
  const appearedSoFar = () =>
    target
      .waitFor({ state: "visible", timeout: 2_000 })
      .then(() => true)
      .catch(() => false);
  for (let i = 0; i < 50 && !(await appearedSoFar()); i++) {
    if (!(await loadMore.isVisible().catch(() => false))) break;
    const before = await page.locator("tbody tr").count();
    await loadMore.click();
    await expect.poll(() => page.locator("tbody tr").count(), { timeout: 15_000 }).toBeGreaterThan(before);
  }
}

// Per-run unique, all-lowercase/digits/dashes after the prefix so every
// generated name satisfies the artifact slugRe (`^[a-z0-9][a-z0-9-]*$`,
// internal/api/admin_artifacts.go). A fixed name would let a re-run pass
// VACUOUSLY against a prior run's leftover rows — the servers.spec.ts lesson
// from v1.16.0, restated in dropdowns.spec.ts and approval.spec.ts.
//
// Undocumented-until-now cross-spec ordering coupling: every OTHER spec file
// in this suite names its rows `e2e-*`, which sorts BEFORE this `pg<digits>-*`
// prefix (`e` < `p`). That is the only reason 105+ rows seeded here never
// pushed another spec's few rows off page 1 when both ran concurrently in
// different workers. It is not enforced anywhere — a future spec naming rows
// `z-*` (sorting AFTER `pg…`) would risk exactly the off-page failure this
// file's own cleanup (below) exists to prevent for ITS OWN re-runs.
const RUN = `pg${Date.now()}`;

test.describe("admin list pagination (real API + real browser)", () => {
  // Registered by each test immediately after a create call succeeds — never
  // deferred to the end of the test — so cleanup still runs even if a LATER
  // step in the same test (a browser interaction, an assertion) times out.
  // An in-test try/finally does NOT run on a Playwright test timeout: the
  // timeout abandons the test function mid-await, so code after it —
  // including a wrapping finally — never resumes. Proven live: seeding 105
  // rows then forcing a 12s timeout leaked all 105 under the old try/finally
  // shape. `afterEach` is a separate lifecycle step Playwright always runs,
  // pass/fail/timeout alike, which is why cleanup lives there now.
  let seededServerIds: string[] = [];
  let seededArtifactIds: string[] = [];

  test.afterEach(async ({ request }) => {
    const token = await adminToken(request);
    await deleteAll(request, token, "/v1/admin/servers", seededServerIds);
    await deleteAll(request, token, "/v1/admin/artifacts", seededArtifactIds);
    seededServerIds = [];
    seededArtifactIds = [];
  });

  test("Load more appends pages of servers; the cursor value round-trips with no dup or gap", async ({
    page,
    request,
  }) => {
    test.setTimeout(180_000);
    const token = await adminToken(request);

    // Seed past the server's default page size (100) so a second page must
    // exist.
    const N = 105;
    const names = Array.from({ length: N }, (_, i) => `${RUN}-srv-${String(i).padStart(3, "0")}`);
    const t0 = Date.now();
    const ids = await mapConcurrent(names, 6, async (name) => {
      const res = await request.post(`${API_BASE}/v1/admin/servers`, {
        headers: { Authorization: `Bearer ${token}` },
        data: {
          name,
          description: "pagination e2e seed",
          transport: "http",
          endpointOrCommand: "https://example.invalid/mcp",
          version: "",
          protocolVersion: "",
          secretRef: "",
          status: "active",
        },
      });
      expect(res.ok(), `seed server ${name}: ${res.status()} ${await res.text()}`).toBeTruthy();
      return ((await res.json()) as { id: string }).id;
    });
    seededServerIds.push(...ids);
    console.log(`[pagination.spec] seeded ${N} servers in ${Date.now() - t0}ms`);

    await login(page, E2E_USER, E2E_USER);
    await gotoAdmin(page, "Servers");

    const loadMore = page.getByRole("button", { name: "Load more", exact: true });
    await expect(loadMore).toBeVisible();

    const ourRowNames = async (): Promise<string[]> => {
      const all = await page.locator("tbody tr td:first-child").allInnerTexts();
      return all.filter((n) => n.startsWith(`${RUN}-srv-`));
    };

    const firstPageOurs = await ourRowNames();
    // First page is capped: it cannot already contain every seeded row (that
    // would mean the list ignored its own default limit).
    expect(firstPageOurs.length, "first page already contains every seeded row").toBeLessThan(N);

    // Click Load more until every seeded row is visible or the control is
    // exhausted, bounded so a broken cursor (stuck re-fetching page 1, or
    // never advancing) fails loudly instead of hanging the test.
    for (let i = 0; i < 6; i++) {
      const before = (await ourRowNames()).length;
      if (before >= N) break;
      if (!(await loadMore.isVisible().catch(() => false))) break;
      await loadMore.click();
      await expect.poll(async () => (await ourRowNames()).length, { timeout: 15_000 }).toBeGreaterThan(before);
    }

    const finalOurs = await ourRowNames();
    // The cursor's VALUE round-tripped: every seeded name shows up, exactly
    // once each. Task 10's unit test only asserts a `cursor=` param is
    // PRESENT on the follow-up request — a hardcoded/wrong cursor value would
    // still satisfy that, either re-serving page 1 forever (duplicates, N
    // never reached within the click budget above) or skipping ahead (a
    // gap). Sorting both sides turns a dup/gap into a readable array diff
    // instead of a bare boolean.
    expect(finalOurs.slice().sort()).toEqual(names.slice().sort());

    // The first page's own rows are still on screen after Load more — pages
    // accumulate, they don't replace (useAdminList's whole reason for being
    // built on useInfiniteQuery instead of component-local accumulator state).
    for (const n of firstPageOurs) {
      await expect(page.getByRole("cell", { name: n, exact: true })).toBeVisible();
    }
  });

  test("artifact list rows are slim by default; the edit form fetches the full artifact", async ({
    page,
    request,
  }) => {
    const token = await adminToken(request);
    const name = `${RUN}-artifact`;
    const body = `BODY-${RUN}`;
    const createRes = await request.post(`${API_BASE}/v1/admin/artifacts`, {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        type: "skill",
        name,
        description: "pagination e2e seed",
        content: `---\nname: ${name}\ndescription: pagination e2e seed\n---\n${body}\n`,
        memoryScope: "",
        memorySeed: "",
        version: "",
        visibility: "org",
      },
    });
    expect(createRes.ok(), `seed artifact: ${createRes.status()} ${await createRes.text()}`).toBeTruthy();
    const created = (await createRes.json()) as { id: string };
    seededArtifactIds.push(created.id);

    // The list response must actually CONTAIN this row before its absence of
    // `body` means anything. Without this, a row pushed off-page by unrelated
    // volume (proven live, see walkArtifactPages's doc comment) would make
    // the next assertion pass for entirely the wrong reason, the exact "a
    // check whose negative result isn't evidence of the thing it claims"
    // shape. Walking every page (instead of assuming page 1) also makes the
    // slimness check itself STRONGER: every page it visits along the way is
    // asserted slim too, not only the one this row happens to land on.
    let sawBody = false;
    const found = await walkArtifactPages(request, token, (pageText) => {
      if (pageText.includes(body)) sawBody = true;
      return pageText.includes(name);
    });
    expect(found, "seeded row is not even in the list response, page/volume issue, not a slimness proof").toBeTruthy();
    expect(sawBody, "artifact content appeared in a slim list page").toBeFalsy();

    // ...but opening the edit form fetches GET /{id} (always the full
    // payload — see useAdminArtifact's doc comment) and shows it.
    await login(page, E2E_USER, E2E_USER);
    await gotoAdmin(page, "Artifacts");
    const editButton = page.getByRole("button", { name: `Edit ${name}`, exact: true });
    await revealRow(page, editButton);
    await editButton.click();
    // Scope the field lookup to the open <form>: row-action buttons carry
    // aria-labels like "Edit <name>"/"Delete <name>", and getByLabel is a
    // case-insensitive SUBSTRING match, so once a row exists an unscoped
    // lookup risks the same strict-mode collision the v1.18.0 e2e defect hit.
    // Every other spec in this suite scopes field lookups to `form` for
    // exactly this reason.
    const form = page.locator("form");
    await expect(form.getByLabel(/content/i)).toHaveValue(new RegExp(body));
  });

  test("editing an unrelated field round-trips memorySeed unchanged (the C8 silent-wipe path)", async ({
    page,
    request,
  }) => {
    const token = await adminToken(request);
    const name = `${RUN}-seed-subagent`;
    const seed = `MEMORYSEED-${RUN}`;
    const createRes = await request.post(`${API_BASE}/v1/admin/artifacts`, {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        type: "subagent",
        name,
        description: "pagination e2e seed (before)",
        content: `---\nname: ${name}\ndescription: pagination e2e seed\n---\nReview Go code.\n`,
        memoryScope: "user",
        memorySeed: seed,
        version: "",
        visibility: "org",
      },
    });
    expect(createRes.ok(), `seed artifact: ${createRes.status()} ${await createRes.text()}`).toBeTruthy();
    const created = (await createRes.json()) as { id: string };
    seededArtifactIds.push(created.id);

    await login(page, E2E_USER, E2E_USER);
    await gotoAdmin(page, "Artifacts");
    const editButton = page.getByRole("button", { name: `Edit ${name}`, exact: true });
    await revealRow(page, editButton);
    await editButton.click();
    const form = page.locator("form");
    // The form must have loaded the FULL artifact (not a slim list row, whose
    // content/memorySeed are both "") before we touch anything — otherwise
    // this test would trivially pass by saving blanks over blanks, proving
    // nothing about the wipe path it exists to guard.
    await expect(form.getByLabel(/content/i)).toHaveValue(/Review Go code/);

    const newDescription = "pagination e2e seed (after edit)";
    await form.getByLabel(/description/i).fill(newDescription);
    // Form-scoped like every other field lookup in this file — the Save
    // button lives inside the same <form> (ArtifactsPage.tsx), so there is no
    // reason for this one lookup to be the unscoped exception.
    await form.getByRole("button", { name: /^save$/i }).click();

    // Wait for the form to close (save resolved) before re-fetching.
    await expect(page.locator("form")).toHaveCount(0, { timeout: 15_000 });

    const getRes = await request.get(`${API_BASE}/v1/admin/artifacts/${created.id}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(getRes.ok()).toBeTruthy();
    const after = (await getRes.json()) as { description: string; memorySeed: string };
    expect(after.description).toBe(newDescription);
    expect(after.memorySeed, "memorySeed was silently wiped by an unrelated-field save").toBe(seed);
  });

  test("review queue renders full content for a pending artifact, and Load more appends past a state+include query string", async ({
    page,
    request,
  }) => {
    test.setTimeout(180_000);
    const token = await adminToken(request);

    // Seed past the review queue's default page size (100; it reuses
    // useAdminList like the other five lists) so a second page must exist.
    const N = 105;
    const names = Array.from({ length: N }, (_, i) => `${RUN}-rvq-${String(i).padStart(3, "0")}`);
    const t0 = Date.now();
    const ids = await mapConcurrent(names, 6, async (name, i) => {
      const body = i === 0 ? `REVIEWBODY-${RUN}` : `filler body ${i}`;
      const res = await request.post(`${API_BASE}/v1/admin/artifacts`, {
        headers: { Authorization: `Bearer ${token}` },
        data: {
          type: "skill",
          name,
          description: "pagination e2e review-queue seed",
          content: `---\nname: ${name}\ndescription: pagination e2e review-queue seed\n---\n${body}\n`,
          memoryScope: "",
          memorySeed: "",
          version: "",
          visibility: "org",
        },
      });
      expect(res.ok(), `create ${name}: ${res.status()} ${await res.text()}`).toBeTruthy();
      return ((await res.json()) as { id: string }).id;
    });
    // Registered immediately after create, BEFORE the submit loop below —
    // if submission (or anything after it) times out, the already-created
    // rows are still tracked for cleanup.
    seededArtifactIds.push(...ids);
    const createMs = Date.now() - t0;

    const t1 = Date.now();
    await mapConcurrent(ids, 6, async (id) => {
      const res = await request.post(`${API_BASE}/v1/admin/artifacts/${id}/submit`, {
        headers: { Authorization: `Bearer ${token}` },
      });
      expect(res.ok(), `submit ${id}: ${res.status()} ${await res.text()}`).toBeTruthy();
    });
    const submitMs = Date.now() - t1;
    console.log(
      `[pagination.spec] seeded+submitted ${N} pending artifacts: create=${createMs}ms submit=${submitMs}ms`,
    );

    await login(page, E2E_USER, E2E_USER);
    await gotoAdmin(page, "Review queue");

    // Content renders end to end (Task 11 review gap: the unit test only
    // asserts the request URL carries include=content — it never feeds real
    // content through the Diff component, so a client-side regression that
    // drops it renders blank and stays green there).
    await expect(page.getByText(`REVIEWBODY-${RUN}`)).toBeVisible();

    const loadMore = page.getByRole("button", { name: "Load more", exact: true });
    await expect(loadMore).toBeVisible();

    const ourCards = () => page.locator(".rounded-xl", { hasText: `${RUN}-rvq-` });
    const before = await ourCards().count();
    expect(before, "first page already contains every seeded pending artifact").toBeLessThan(N);

    await loadMore.click();
    // Growth is the assertion that actually catches a broken `?`-vs-`&` join
    // on `?state=pending&include=content` — under that break the queue never
    // grows, and `expect.poll` times out with a diagnostic count. An
    // immediate `toHaveCount(0)` check on the error text right after the
    // click is a no-op race (it resolves before a 400 has any chance to
    // surface), so the error-text check comes AFTER growth is confirmed —
    // belt-and-suspenders against a lingering stale error banner, not the
    // primary signal.
    await expect.poll(() => ourCards().count(), { timeout: 15_000 }).toBeGreaterThan(before);
    await expect(page.getByText(/failed to load more of the review queue/i)).toHaveCount(0);
  });
});
