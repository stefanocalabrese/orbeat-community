import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

// Role deletion cascade (docs/specs/2026-08-11-orbeat-role-deletion-design.md,
// docs/plans/orbeat-role-deletion-2026-08-11.md Task 5).
//
// DELETE /v1/admin/roles/{id} cascades: deleting a role revokes every
// entitlement and artifact_entitlement granted to it (ON DELETE CASCADE, two
// FK paths each — see store.DeleteRole's doc comment). That cascade fires
// entirely server-side. Asserting only that the role's row disappeared from
// the Roles page table would prove nothing about it — the portal never
// renders entitlements on that page at all, so a broken cascade (or one that
// only *looked* like it worked because the UI just removed the row locally)
// would leave this spec green while grants silently survived. The decisive
// assertion has to go back through the real API, independent of the browser.
//
// There is no shared `./helpers` module in this suite — every spec defines
// `login()` / `adminToken()` locally (concurrency.spec.ts's doc comment).
// Reused verbatim below.

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

/** Verbatim from concurrency.spec.ts / pagination.spec.ts. */
async function adminToken(request: APIRequestContext, user = "boss", pass = "boss"): Promise<string> {
  const res = await request.post(KC_TOKEN_URL, {
    form: { grant_type: "password", client_id: "orbeat-cli", username: user, password: pass },
  });
  expect(res.ok(), `token request failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  const body = (await res.json()) as { access_token?: string };
  expect(body.access_token, "no access_token in token response").toBeTruthy();
  return body.access_token as string;
}

// Per-run unique (Date.now()) so a prior run's leftover role/server/artifact
// can never make an assertion in here pass vacuously (the v1.16.0 lesson).
// All lowercase letters/digits/dashes — the artifact name must satisfy the
// server's slug pattern (^[a-z0-9][a-z0-9-]*$, internal/api/admin_artifacts.go)
// and the role/server names are built from the same prefix for consistency,
// not because either is format-constrained.
const RUN = `e2erole${Date.now()}`;
const ROLE_NAME = `${RUN}-role`;
const SERVER_NAME = `${RUN}-srv`;
const ARTIFACT_NAME = `${RUN}-art`;

test.describe("role deletion cascades to its grants (real API + real browser)", () => {
  let roleId = "";
  let serverId = "";
  let artifactId = "";
  let entitlementId = "";
  let artifactEntitlementId = "";

  // Registered in afterEach, not an in-test try/finally: a Playwright test
  // timeout abandons the test function mid-await, so code after it —
  // including a wrapping finally — never runs. afterEach is a separate
  // lifecycle step Playwright always executes, pass/fail/timeout alike
  // (proven live during v1.22.0's pagination work). Every delete here is
  // best-effort: by the time this runs, the role/entitlement/artifact-
  // entitlement are normally already gone via the cascade under test, so a
  // 404 on those is the expected happy path, not a cleanup failure. The
  // server and artifact are NOT touched by the role-deletion cascade (only
  // the entitlement rows joining them to the role are), so they always need
  // an explicit delete here.
  test.afterEach(async ({ request }) => {
    const token = await adminToken(request);
    const headers = { Authorization: `Bearer ${token}` };
    for (const [base, id] of [
      ["/v1/admin/artifact-entitlements", artifactEntitlementId],
      ["/v1/admin/entitlements", entitlementId],
      ["/v1/admin/roles", roleId],
      ["/v1/admin/artifacts", artifactId],
      ["/v1/admin/servers", serverId],
    ] as const) {
      if (!id) continue;
      await request.delete(`${API_BASE}${base}/${id}`, { headers });
    }
    roleId = "";
    serverId = "";
    artifactId = "";
    entitlementId = "";
    artifactEntitlementId = "";
  });

  test("deleting a role through the portal revokes its server and artifact grants server-side", async ({
    page,
    request,
  }) => {
    const token = await adminToken(request);
    const headers = { Authorization: `Bearer ${token}` };

    // --- Seed via the real API: a role, a server grant, and an artifact grant. ---
    const roleRes = await request.post(`${API_BASE}/v1/admin/roles`, {
      headers,
      data: { name: ROLE_NAME },
    });
    expect(roleRes.ok(), `create role: ${roleRes.status()} ${await roleRes.text()}`).toBeTruthy();
    roleId = ((await roleRes.json()) as { id: string }).id;

    const serverRes = await request.post(`${API_BASE}/v1/admin/servers`, {
      headers,
      data: {
        name: SERVER_NAME,
        description: "role-deletion e2e seed",
        transport: "http",
        endpointOrCommand: "https://example.invalid/mcp",
        version: "",
        protocolVersion: "",
        secretRef: "",
        status: "active",
      },
    });
    expect(serverRes.ok(), `create server: ${serverRes.status()} ${await serverRes.text()}`).toBeTruthy();
    serverId = ((await serverRes.json()) as { id: string }).id;

    const entRes = await request.post(`${API_BASE}/v1/admin/entitlements`, {
      headers,
      data: { roleId, mcpServerId: serverId, allowedTools: null },
    });
    expect(entRes.ok(), `create entitlement: ${entRes.status()} ${await entRes.text()}`).toBeTruthy();
    entitlementId = ((await entRes.json()) as { id: string }).id;

    const artifactRes = await request.post(`${API_BASE}/v1/admin/artifacts`, {
      headers,
      data: {
        type: "skill",
        name: ARTIFACT_NAME,
        description: "role-deletion e2e seed",
        content: `---\nname: ${ARTIFACT_NAME}\ndescription: role-deletion e2e seed\n---\nbody\n`,
        memoryScope: "",
        memorySeed: "",
        version: "",
        visibility: "org",
      },
    });
    expect(artifactRes.ok(), `create artifact: ${artifactRes.status()} ${await artifactRes.text()}`).toBeTruthy();
    artifactId = ((await artifactRes.json()) as { id: string }).id;

    const artEntRes = await request.post(`${API_BASE}/v1/admin/artifact-entitlements`, {
      headers,
      data: { roleId, artifactId },
    });
    expect(
      artEntRes.ok(),
      `create artifact entitlement: ${artEntRes.status()} ${await artEntRes.text()}`,
    ).toBeTruthy();
    artifactEntitlementId = ((await artEntRes.json()) as { id: string }).id;

    // --- Delete the role through the real browser. ---
    await login(page, "boss", "boss");
    await gotoAdmin(page, "Roles");

    const row = page.locator("li", { hasText: ROLE_NAME });
    await expect(row).toBeVisible();

    // Registered BEFORE the click that triggers it, per Playwright's dialog
    // handling requirement — a handler attached after window.confirm() has
    // already fired never sees it.
    page.once("dialog", (dialog) => dialog.accept());

    // Registered before the click for the same reason as concurrency.spec.ts:
    // this waits for the BROWSER's own DELETE, not an out-of-band API call, so
    // a CORS/routing regression fails here as a timeout rather than being
    // silently unobserved.
    const deleteResponse = page.waitForResponse(
      (r) => r.request().method() === "DELETE" && r.url() === `${API_BASE}/v1/admin/roles/${roleId}`,
      { timeout: 15_000 },
    );

    // aria-label is `Delete ${r.name}` (RolesPage.tsx). getByRole's accessible-
    // name match is substring by default, and ROLE_NAME is built from the same
    // RUN prefix as every other name in this file — `exact: true` is what
    // makes this unambiguous, not the name's uniqueness alone (the exact
    // hazard that broke the suite in v1.18.0: a shorter label can substring-
    // match a longer one sharing a prefix).
    await row.getByRole("button", { name: `Delete ${ROLE_NAME}`, exact: true }).click();

    const delRes = await deleteResponse;
    expect(delRes.status(), "the browser's DELETE did not succeed").toBe(200);
    const delBody = (await delRes.json()) as {
      entitlementsRevoked: number;
      artifactEntitlementsRevoked: number;
    };
    expect(delBody).toEqual({ entitlementsRevoked: 1, artifactEntitlementsRevoked: 1 });

    await expect(row).toHaveCount(0);

    // --- Assert via the API that the role is gone AND both grants are gone. ---
    // Asserting only that the row vanished from the table would be asserting
    // the wrong thing: the cascade is server-side behaviour, and the portal
    // never lists entitlements on the Roles page at all. Re-deleting each id
    // is the same idiom the store/API tests use to prove absence (store's
    // TestDeleteRoleNotFound, the sibling deletes' RowsAffected()==0 ->
    // ErrNotFound -> 404 mapping) — a resource that still existed would 204,
    // one that is truly gone 404s.
    const roleGone = await request.delete(`${API_BASE}/v1/admin/roles/${roleId}`, { headers });
    expect(roleGone.status(), "the role still exists after deletion").toBe(404);

    const entGone = await request.delete(`${API_BASE}/v1/admin/entitlements/${entitlementId}`, { headers });
    expect(entGone.status(), "the server entitlement survived the role deletion").toBe(404);

    const artEntGone = await request.delete(
      `${API_BASE}/v1/admin/artifact-entitlements/${artifactEntitlementId}`,
      { headers },
    );
    expect(artEntGone.status(), "the artifact entitlement survived the role deletion").toBe(404);
  });
});
