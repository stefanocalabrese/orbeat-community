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
