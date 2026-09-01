import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

// Audit filtering (?actor, ?action, ?decision on GET /v1/admin/audit).
//
// The portal's own unit tests mock fetch, so they prove the page ASKS for the
// right URL and renders whatever comes back. They cannot prove the server
// narrows anything, and that seam is where this repo has been bitten before: a
// server-status dropdown stayed green in jsdom while the feature was dead
// (CHANGELOG v1.16.0). This spec drives the real stack.
//
// The decisive shape is two DISJOINT actions: filtering by one must make the
// other disappear. Asserting only that the filtered-for row is present would
// pass unchanged on a server that ignores the parameter and returns everything.
//
// Every existence check below goes through the SAME ?action= filter the
// narrowing check exercises, never an unfiltered first page. The audit log is
// one shared, size-capped (default 100 rows) table for the whole e2e run:
// pagination.spec.ts alone writes 300+ newer rows (servers, artifacts,
// submits) in well under a second when it runs concurrently, which is enough
// to push either of this test's two rows off an unfiltered page before this
// test ever looks, reproduced live by running this file alongside
// pagination.spec.ts, which failed the unfiltered role.create check on the
// first attempt every time. Filtering by action is immune to that: it only
// competes with OTHER role.create/server.create rows, and nothing else in
// this suite floods either action.
//
// There is no shared ./helpers module in this suite: every spec defines
// login() / adminToken() locally (concurrency.spec.ts's doc comment).

async function login(page: Page, user: string, pass: string) {
  await page.goto("/");
  await page.getByRole("button", { name: /sign in/i }).click();
  await page.fill("#username", user);
  await page.fill("#password", pass);
  await page.click("#kc-login");
  await page.waitForURL(/\/catalog/, { timeout: 30_000 });
  await expect(page.getByRole("heading", { name: /^catalog$/i })).toBeVisible({ timeout: 15_000 });
}

const API_BASE = "http://localhost:8080";
const KC_TOKEN_URL = "http://localhost:8088/realms/orbeat/protocol/openid-connect/token";

/**
 * This spec's own Keycloak identity. It writes two audit rows and reads them
 * back, so it is not what loads the bucket, but it moves anyway: the gate in
 * internal/deploy derives its subject set from source (every spec defining
 * adminToken), and carving out the cheap specs would mean a hand-maintained
 * exemption list, which is the defect one level up.
 *
 * internal/ratelimit keys its token bucket on subject + azp, so every spec
 * that stays on `boss` shares one bucket with all the others. One constant
 * feeds both the browser login and the API token so the two can never drift
 * apart, which would silently put half this spec's traffic back on the
 * shared bucket.
 */
const E2E_USER = "e2e-auditfilters";

async function adminToken(request: APIRequestContext, user = E2E_USER, pass = E2E_USER): Promise<string> {
  const res = await request.post(KC_TOKEN_URL, {
    form: { grant_type: "password", client_id: "orbeat-cli", username: user, password: pass },
  });
  expect(res.ok(), `token request failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  const body = (await res.json()) as { access_token?: string };
  expect(body.access_token, "no access_token in token response").toBeTruthy();
  return body.access_token as string;
}

// Per-run unique so a previous run's rows can never satisfy an assertion here.
const RUN = `e2eaudit${Date.now()}`;

test("the audit filters narrow server-side, and the API refuses an impossible decision", async ({ page, request }) => {
  const token = await adminToken(request);
  const auth = { Authorization: `Bearer ${token}`, "Content-Type": "application/json" };

  // Two admin actions, each writing an audit event with a DIFFERENT action:
  // server.create and role.create.
  const srv = await request.post(`${API_BASE}/v1/admin/servers`, {
    headers: auth,
    data: { name: `${RUN}-srv`, description: "audit filter spec", transport: "http", endpointOrCommand: "https://example.invalid/mcp", status: "active" },
  });
  expect(srv.ok(), `create server: ${srv.status()} ${await srv.text()}`).toBeTruthy();
  const role = await request.post(`${API_BASE}/v1/admin/roles`, { headers: auth, data: { name: `${RUN}-role` } });
  expect(role.ok(), `create role: ${role.status()} ${await role.text()}`).toBeTruthy();

  // A decision outside the column's CHECK is a 400, not an empty page: the
  // value can never exist, so reporting "nothing happened" would be a lie.
  const bad = await request.get(`${API_BASE}/v1/admin/audit?decision=maybe`, { headers: auth });
  expect(bad.status(), "an unknown ?decision should be refused").toBe(400);

  await login(page, E2E_USER, E2E_USER);
  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Audit" }).click();
  await expect(page.getByRole("heading", { name: /audit log/i })).toBeVisible();

  const filterByAction = async (action: string) => {
    await page.getByLabel("Filter by action").fill(action);
    await page.getByRole("button", { name: /apply filters/i }).click();
  };

  // Each action's existence, proven via its OWN filter rather than an
  // unfiltered first page (see the file-level comment for why).
  await filterByAction("server.create");
  await expect(page.getByText("server.create").first()).toBeVisible({ timeout: 15_000 });

  await filterByAction("role.create");
  await expect(page.getByText("role.create").first()).toBeVisible({ timeout: 15_000 });
  // The disjoint half: server.create, independently proven to exist just
  // above, must be gone entirely from a query narrowed to role.create. The
  // only way both hold is that the server actually narrowed by `action` ,
  // not that server.create merely happened to be invisible already.
  await expect(page.getByText("server.create")).toHaveCount(0);

  // Clear resets the filter draft itself, checked directly rather than via
  // the shared table.
  await page.getByRole("button", { name: /^clear$/i }).click();
  await expect(page.getByLabel("Filter by action")).toHaveValue("");

  // ...and server.create is discoverable again once re-filtered for it,
  // proving role.create's exclusion above was the filter's doing and not a
  // permanent disappearance.
  await filterByAction("server.create");
  await expect(page.getByText("server.create").first()).toBeVisible({ timeout: 15_000 });
});
