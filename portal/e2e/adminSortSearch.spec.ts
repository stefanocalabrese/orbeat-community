import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

// Task 5 (docs/plans/orbeat-admin-search-sort-2026-08-27.md): sortable admin
// list headers + a ?q= search box. Portal unit tests mock `fetch`, so they can
// prove the CLIENT drops a cursor on a sort/search change but cannot prove the
// real API actually 400s a replayed one, or that a real browser never sends
// that replay in the first place. This spec drives the real API through the
// real browser: the one gate in this repo that can catch a stale-cursor
// regression here, following pagination.spec.ts's helpers rather than
// reinventing them (verbatim `login`/`adminToken`/`mapConcurrent`/`deleteAll`
// below), including its "walk by Load-more, never assume page one" discipline
// for a list other specs write to concurrently.

/** Verbatim from pagination.spec.ts / servers.spec.ts / dropdowns.spec.ts. */
async function login(page: Page, user: string, pass: string) {
  await page.goto("/");
  await page.getByRole("button", { name: /sign in/i }).click();
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
 * This spec's own Keycloak identity. Its 105-server seed and teardown put
 * about 210 direct API calls on a single bucket, the second heaviest in the
 * suite after pagination.spec.ts (see deploy/docker-compose.yml's
 * ORBEAT_RATELIMIT_BURST derivation).
 *
 * internal/ratelimit keys its token bucket on subject + azp, so every spec
 * that stays on `boss` shares one bucket with all the others. One constant
 * feeds both the browser login and the API token so the two can never drift
 * apart, which would silently put half this spec's traffic back on the
 * shared bucket.
 */
const E2E_USER = "e2e-sortsearch";

/** Verbatim from pagination.spec.ts. */
async function adminToken(request: APIRequestContext, user = E2E_USER, pass = E2E_USER): Promise<string> {
  const res = await request.post(KC_TOKEN_URL, {
    form: { grant_type: "password", client_id: "orbeat-cli", username: user, password: pass },
  });
  expect(res.ok(), `token request failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  const body = (await res.json()) as { access_token?: string };
  expect(body.access_token, "no access_token in token response").toBeTruthy();
  return body.access_token as string;
}

/** Verbatim from pagination.spec.ts. */
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

/** Verbatim from pagination.spec.ts. */
async function deleteAll(request: APIRequestContext, token: string, base: string, ids: string[]): Promise<void> {
  if (ids.length === 0) return;
  await mapConcurrent(ids, 6, async (id) => {
    const res = await request.delete(`${API_BASE}${base}/${id}`, { headers: { Authorization: `Bearer ${token}` } });
    expect(res.ok(), `delete ${base}/${id}: ${res.status()} ${await res.text()}`).toBeTruthy();
  });
}

/**
 * Reads every visible row's name (first cell) whose text starts with `prefix`,
 * clicking "Load more" until either every one of `expectedCount` names has
 * appeared or the control exhausts: the same bounded-by-exhaustion,
 * poll-on-growth discipline as pagination.spec.ts's `revealRow`, generalized
 * to collect names rather than wait for one. Never assumes page one: this
 * list is shared with every other spec in the suite, run concurrently.
 */
async function collectOurRows(page: Page, prefix: string, expectedCount: number): Promise<string[]> {
  const ourRows = async () => {
    const all = await page.locator("tbody tr td:first-child").allInnerTexts();
    return all.filter((n) => n.startsWith(prefix));
  };
  // A sort/search control change re-fetches page one under a brand new query
  // key (queries.ts's useAdminList), and ServersPage renders "Loading…"
  // instead of the table while it is in flight: the whole tbody, "Load
  // more" included, is briefly absent. Waiting for a row here first is what
  // keeps that transient state from being misread as exhaustion below.
  await page
    .locator("tbody tr")
    .first()
    .waitFor({ state: "visible", timeout: 15_000 })
    .catch(() => {});
  const loadMore = page.getByRole("button", { name: "Load more", exact: true });
  for (let i = 0; i < 10; i++) {
    if ((await ourRows()).length >= expectedCount) break;
    if (!(await loadMore.isVisible().catch(() => false))) break;
    const before = (await ourRows()).length;
    await loadMore.click();
    await expect.poll(async () => (await ourRows()).length, { timeout: 15_000 }).toBeGreaterThan(before);
  }
  return ourRows();
}

// srch<ms>- : lowercase+digits+dash, distinct from every other spec's `e2e-*`
// prefix and from pagination.spec.ts's `pg<ms>-*`, so a re-run never passes
// vacuously against a prior run's leftover rows (the servers.spec.ts lesson).
const RUN = `srch${Date.now()}`;

test.describe("admin sort + search (real API + real browser)", () => {
  let seededServerIds: string[] = [];

  // Same reasoning as pagination.spec.ts: registered right after each create
  // call succeeds, not deferred to a try/finally: a Playwright test timeout
  // abandons the test function mid-await and never resumes a wrapping
  // finally, so afterEach is the lifecycle step that actually always runs.
  test.afterEach(async ({ request }) => {
    const token = await adminToken(request);
    await deleteAll(request, token, "/v1/admin/servers", seededServerIds);
    seededServerIds = [];
  });

  test("sorting the Name column reverses order and never replays a stale cursor as a 400; searching narrows to a name match with no cursor", async ({
    page,
    request,
  }) => {
    test.setTimeout(180_000);
    const token = await adminToken(request);

    // Seeded past the server list's default page size (100) so BOTH the
    // ascending and the descending walk each need more than one page: the
    // scenario where a cursor minted under one order, replayed under the
    // other, would 400 on the real API (8935bb9, 8e0636c) if the portal ever
    // failed to drop it on the sort click.
    const N = 105;
    const names = Array.from({ length: N }, (_, i) => `${RUN}-${String(i).padStart(3, "0")}`);
    const ids = await mapConcurrent(names, 6, async (name) => {
      const res = await request.post(`${API_BASE}/v1/admin/servers`, {
        headers: { Authorization: `Bearer ${token}` },
        data: {
          name,
          description: "sort/search e2e seed",
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

    await login(page, E2E_USER, E2E_USER);
    await gotoAdmin(page, "Servers");

    // Ascending (the default): our rows appear in the order we named them.
    const ascending = await collectOurRows(page, `${RUN}-`, N);
    expect(ascending).toEqual(names);

    // Clicking Name flips to descending. If the portal ever kept the
    // ascending walk's accumulated cursor across this click (the mutant this
    // whole slice exists to prevent), the API would refuse the next "Load
    // more" with 400 and this page would show its error text instead of
    // finishing the walk.
    await page.getByRole("button", { name: "Name", exact: true }).click();
    const descending = await collectOurRows(page, `${RUN}-`, N);
    expect(descending).toEqual(names.slice().reverse());
    await expect(page.getByText(/failed to load (more )?servers/i)).toHaveCount(0);

    // Searching narrows to the one matching row and drops whatever cursor the
    // descending walk above had accumulated. Searched while still sorted
    // descending, so this also proves order and search compose rather than
    // one silently resetting the other.
    const target = names[42];
    if (!target) throw new Error("expected a 43rd seeded name");
    await page.getByRole("searchbox", { name: "Search servers" }).fill(target);
    // POLL until the list has actually narrowed, rather than waiting for the
    // target cell to appear. Two changes landed together (audit B35) and each
    // one alone breaks the old wait:
    //
    //   - the search box is debounced by 300ms, so the request does not even
    //     leave the browser on the keystroke that fill() ends with;
    //   - useAdminList now sets placeholderData: keepPreviousData, so the
    //     previous query's rows STAY on screen while the new one is in flight
    //     instead of the tbody collapsing to "Loading…".
    //
    // The old assertion waited for a cell named `target` to be visible. That
    // row was already on screen from the descending walk above, so it passed
    // instantly against the UNFILTERED list and the count assertion below then
    // read all 104 rows. The "Loading…" gap had been silently guaranteeing that
    // this wait could not observe stale rows; removing it removed the
    // guarantee, which is this repo's own lesson about asking what the thing
    // you replaced was implicitly providing.
    await expect
      .poll(
        async () => {
          const all = await page.locator("tbody tr td:first-child").allInnerTexts();
          return all.filter((n) => n.startsWith(`${RUN}-`));
        },
        { timeout: 15_000 },
      )
      .toEqual([target]);
    await expect(page.getByRole("button", { name: "Load more", exact: true })).toHaveCount(0);
  });
});
