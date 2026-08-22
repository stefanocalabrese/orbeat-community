import { expect, test, type Page } from "@playwright/test";

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

// Drives the Servers admin form against the REAL API — the API↔portal seam.
//
// This exists because that seam broke silently: the API gained an
// `mcp_server.status` CHECK of active|disabled while the portal's dropdown still
// offered active|inactive, so the UI's only non-active option 400'd and there
// was no way to disable a server at all. Nothing caught it — the portal's own
// unit tests mock `fetch` (green while broken), smoke.sh only ever sends
// "active", and the Go handler tests covered "Active"/"bogus" but not
// "inactive", the one string the portal actually sent.
//
// So: submit each status the dropdown offers, through the browser, to the real
// API, and assert the server accepted it. A value the UI offers but the API
// rejects fails here — which is exactly the drift that got through.

test("every status the Servers dropdown offers is accepted by the real API", async ({ page }) => {
  await login(page, "boss", "boss");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Servers" }).click();

  await page.getByRole("button", { name: /new server/i }).click();

  // Scope FORM-FIELD lookups to the open form. Row-action buttons carry
  // aria-labels like "Edit <name>"/"Delete <name>", and getByLabel matches
  // case-insensitive substrings — so once a row named e.g. e2e-status-* exists,
  // an unscoped getByLabel("Status") also matches those buttons (strict-mode
  // violation). The form is a <form> element; the row buttons live in a <table>
  // outside it, so scoping the field lookups to the form disambiguates.
  const form = page.locator("form");

  // Read the options the UI actually offers, rather than hard-coding them here —
  // hard-coding would let the dropdown drift without this test noticing.
  const statuses = await form
    .getByLabel("Status")
    .locator("option")
    .evaluateAll((opts) => opts.map((o) => (o as HTMLOptionElement).value));

  // Guard: if the selector ever stops matching, fail loudly instead of passing
  // vacuously over an empty list.
  expect(statuses.length, "no status options found — selector drifted?").toBeGreaterThan(1);
  expect(statuses).toContain("active");

  // Unique per run. A fixed name would make a re-run pass VACUOUSLY: the create
  // would 409 on the duplicate, but the row from the previous run is still on
  // screen, so the visibility assertion below would succeed while proving
  // nothing. (Observed: a re-run "passed" in 500ms without creating anything.)
  const run = Date.now();

  // Submit one server per offered status. The API rejecting a value surfaces as
  // an inline error and the row never appears, so a visible row IS the proof the
  // server accepted what the UI sent.
  for (const status of statuses) {
    const name = `e2e-status-${status}-${run}`;

    await page.getByRole("button", { name: /new server/i }).click();
    await form.getByLabel("Name").fill(name);
    await form.getByLabel("Description").fill(`e2e status-seam server (${status})`);
    await form.getByLabel("Endpoint / command").fill("http://upstream:9000/mcp");
    await form.getByLabel("Status").selectOption(status);
    await page.getByRole("button", { name: /^create$/i }).click();

    await expect(
      page.getByText(name).first(),
      `the API rejected status="${status}", which the portal's own dropdown offers`,
    ).toBeVisible({ timeout: 15_000 });
  }
});
