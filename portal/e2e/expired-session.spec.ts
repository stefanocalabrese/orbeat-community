import { expect, test, type Page } from "@playwright/test";

async function login(page: Page, user: string, pass: string) {
  await page.goto("/");
  await page.getByRole("button", { name: /sign in/i }).click();
  // Keycloak login page (external origin, localhost:8088)
  await page.fill("#username", user);
  await page.fill("#password", pass);
  await page.click("#kc-login");
  await page.waitForURL(/\/catalog/, { timeout: 30_000 });
  await expect(page.getByRole("heading", { name: /^catalog$/i })).toBeVisible({
    timeout: 15_000,
  });
}

// Drives the expired-session seam through the real browser.
//
// This exists because a dead Keycloak session used to render as a LIE: every
// admin page discarded its query error, so TanStack Query's `data ?? []`
// produced an empty table — "no servers" — with no failure indication and no
// path back to login. Now a 401 must (a) fire the single-shot unauthorized
// handler, which re-initiates the OIDC flow (signinRedirect toward the
// Keycloak authority), and (b) failing that, render the QueryGate error panel.
// Either outcome is a pass; the silent empty table is the only failure mode.

test("expired session on an admin page re-initiates login or shows the error panel — never an empty table", async ({
  page,
}) => {
  await login(page, "boss", "boss");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Servers" }).click();
  await expect(page.getByRole("heading", { name: /mcp servers/i })).toBeVisible(
    { timeout: 15_000 },
  );

  // From here on, every admin API call is unauthorized — as if the Keycloak
  // session died while the tab was open. The portal calls the API cross-origin
  // (localhost:8080), so the fulfilled responses need CORS headers and the
  // Authorization-header preflight must be answered, or the browser would
  // surface a network error instead of the 401 under test.
  const cors = {
    "access-control-allow-origin": "*",
    "access-control-allow-headers": "authorization, content-type",
    "access-control-allow-methods": "GET, POST, PUT, DELETE, OPTIONS",
  };
  await page.route("**/v1/admin/**", (route) => {
    if (route.request().method() === "OPTIONS") {
      return route.fulfill({ status: 204, headers: cors });
    }
    return route.fulfill({
      status: 401,
      headers: { ...cors, "content-type": "application/json" },
      body: JSON.stringify({ error: { message: "session expired" } }),
    });
  });

  // Arm both outcome observers BEFORE triggering the refetch so a fast
  // signinRedirect can't slip past the request listener.
  //
  // Primary: the 401 handler fires login() -> signinRedirect, observable as a
  // request to the Keycloak authorization endpoint. Fallback: the QueryGate
  // error panel renders. Promise.any resolves on whichever happens first and
  // rejects only if BOTH time out — deterministic on either outcome.
  const outcome = Promise.any([
    page
      .waitForRequest(
        (r) => r.url().includes("/protocol/openid-connect/auth"),
        {
          timeout: 20_000,
        },
      )
      .then(() => "re-login" as const),
    page
      .getByText(/failed to load servers/i)
      .waitFor({ state: "visible", timeout: 20_000 })
      .then(() => "error-panel" as const),
  ]).catch(() => "empty-table-lie" as const);

  // Reload to re-run the admin queries against the now-dead session. Wait only
  // for the navigation to commit: the signinRedirect the 401 triggers may
  // interrupt the load event itself, and that interruption is the success case.
  await page.reload({ waitUntil: "commit" });

  expect(
    await outcome,
    "an expired session must re-initiate login or show the error panel — never render as an empty table",
  ).not.toBe("empty-table-lie");
});

// The MUTATION half of the same seam, and the reason it is a separate test:
// the one above reloads the page, which issues GETs only, so it passed for
// months while MutationCache.onError carried no 401 arm at all (audit A17).
// An admin whose Keycloak session died while a form was open got an inline
// "HTTP 401" under the Create button and nothing else, and Roles, Servers,
// Entitlements and Virtual keys run no polling query, so nothing on screen
// ever contradicted a console that looked healthy.
//
// Only NON-GET admin calls are intercepted here. That is deliberate: leaving
// the reads working reproduces exactly that shape, a page rendering its table
// normally while every write is refused. Routing everything to 401, the way
// the test above does, would let a query fire the handler and the assertion
// would pass with the mutation arm deleted.
//
// NOT YET EXECUTED, as of the commit that added it: written with no compose
// stack available, so it has passed typecheck and lint and nothing more. An
// unrun spec is not coverage (the v1.14.1 lesson), which is why the same
// mechanism is also pinned by src/api/client.unauthorized.test.tsx, a unit
// gate that was red-proven.

test("an expired session on an admin WRITE re-initiates login, not just an inline error", async ({
  page,
}) => {
  await login(page, "boss", "boss");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Roles" }).click();
  await expect(page.getByRole("heading", { name: /^roles$/i })).toBeVisible({
    timeout: 15_000,
  });

  const cors = {
    "access-control-allow-origin": "*",
    "access-control-allow-headers": "authorization, content-type",
    "access-control-allow-methods": "GET, POST, PUT, DELETE, OPTIONS",
  };
  await page.route("**/v1/admin/**", (route) => {
    const method = route.request().method();
    if (method === "OPTIONS") {
      return route.fulfill({ status: 204, headers: cors });
    }
    if (method === "GET") {
      return route.continue(); // reads keep working: the page stays healthy
    }
    return route.fulfill({
      status: 401,
      headers: { ...cors, "content-type": "application/json" },
      body: JSON.stringify({ error: { message: "session expired" } }),
    });
  });

  // Armed before the click so a fast redirect cannot slip past the listener.
  const reLogin = page
    .waitForRequest((r) => r.url().includes("/protocol/openid-connect/auth"), {
      timeout: 20_000,
    })
    .then(() => "re-login" as const)
    .catch(() => "inline-error-only" as const);

  await page.getByLabel("Role name").fill(`e2e401${Date.now()}`);
  await page.getByRole("button", { name: /^create$/i }).click();

  expect(
    await reLogin,
    "a 401 on a write must re-initiate login; rendering only an inline message leaves the admin on a console where every write silently fails",
  ).toBe("re-login");
});
