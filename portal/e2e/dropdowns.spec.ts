import { expect, test, type Locator, type Page } from "@playwright/test";

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

// Dropdown-drift guard, generalized. servers.spec.ts proved ONE dropdown
// (server status) against the real API after a CHECK-constraint added to the
// API silently diverged from the portal's options. That failure class recurs
// for every enum select the UI offers but no test submits: the next
// CHECK-constraint drain recreates the v1.16.0 incident. This spec covers the
// remaining enum dropdowns — artifact type × visibility, subagent memoryScope,
// server transport — by the same method:
//
//   1. read the options FROM THE DOM (hard-coding them here would let the UI
//      drift without this test noticing — the whole point),
//   2. submit each through the browser to the REAL API,
//   3. assert acceptance: a value the UI offers but the API rejects surfaces
//      as an inline error and the row never appears, so a visible row IS the
//      proof the server accepted what the UI sent.

// Per-run unique suffix (all-digits → slug-safe). A fixed name would let a
// re-run pass VACUOUSLY: the create 409s on the duplicate, but the prior run's
// row is still on screen, so the visibility assertion succeeds while proving
// nothing (the servers.spec.ts lesson).
const run = Date.now();

// Scope reads/fills to the open <form>. Row-action buttons carry aria-labels
// like "Edit <name>"/"Delete <name>", and getByLabel is a case-insensitive
// substring match — so once a row whose name embeds a field word exists (e.g.
// e2e-dd-*-transport-*), an unscoped getByLabel("Transport") also matches those
// buttons (strict-mode violation). The row buttons live in a <table> outside
// the form, so scoping field lookups to the form disambiguates.
async function optionValues(form: Locator, label: string): Promise<string[]> {
  return form
    .getByLabel(label)
    .locator("option")
    .evaluateAll((opts) => opts.map((o) => (o as HTMLOptionElement).value));
}

async function gotoAdmin(page: Page, section: string) {
  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: section }).click();
}

// skill/subagent content must be valid YAML frontmatter (name + description);
// a rule's content is plain markdown delivered verbatim into AGENTS.md.
function artifactContent(type: string, name: string): string {
  if (type === "rule") return "Always run gofmt before committing Go code.";
  return `---\nname: ${name}\ndescription: e2e dropdown-drift artifact\n---\nRun gofmt on all Go files.`;
}

test("every artifact type × visibility combo the form offers is accepted by the real API", async ({ page }) => {
  await login(page, "boss", "boss");
  await gotoAdmin(page, "Artifacts");

  // Read the options the UI actually offers rather than hard-coding them.
  await page.getByRole("button", { name: /new artifact/i }).click();
  const form = page.locator("form");
  const types = await optionValues(form, "Type");
  const visibilities = await optionValues(form, "Visibility");

  // Guard: fail loudly if a selector stops matching instead of passing
  // vacuously over an empty list.
  expect(types.length, "no type options found — selector drifted?").toBeGreaterThan(1);
  expect(visibilities.length, "no visibility options found — selector drifted?").toBeGreaterThan(1);
  for (const t of ["skill", "subagent", "rule"]) expect(types).toContain(t);
  for (const v of ["org", "role"]) expect(visibilities).toContain(v);

  for (const type of types) {
    for (const visibility of visibilities) {
      const name = `e2e-dd-${type}-${visibility}-${run}`;

      await page.getByRole("button", { name: /new artifact/i }).click();
      await form.getByLabel("Type").selectOption(type);
      await form.getByLabel("Name").fill(name);
      await form.getByLabel("Description").fill("e2e dropdown-drift artifact");
      await form.getByLabel("Content").fill(artifactContent(type, name));
      await form.getByLabel("Visibility").selectOption(visibility);
      await page.getByRole("button", { name: /^create$/i }).click();

      await expect(
        page.getByText(name).first(),
        `the API rejected type="${type}" visibility="${visibility}", which the Artifacts form offers`,
      ).toBeVisible({ timeout: 15_000 });
    }
  }
});

test("every memoryScope a subagent form offers is accepted by the real API", async ({ page }) => {
  await login(page, "boss", "boss");
  await gotoAdmin(page, "Artifacts");

  // memoryScope is subagent-only; select the type first, then read its options.
  await page.getByRole("button", { name: /new artifact/i }).click();
  const form = page.locator("form");
  await form.getByLabel("Type").selectOption("subagent");
  const scopes = await optionValues(form, "Memory scope");

  expect(scopes.length, "no memory-scope options found — selector drifted?").toBeGreaterThan(1);
  for (const s of ["", "user", "project", "local"]) expect(scopes).toContain(s);

  for (const scope of scopes) {
    const label = scope === "" ? "off" : scope;
    const name = `e2e-dd-scope-${label}-${run}`;

    await page.getByRole("button", { name: /new artifact/i }).click();
    await form.getByLabel("Type").selectOption("subagent");
    await form.getByLabel("Memory scope").selectOption(scope);
    // The seed field appears only for user/project scope; a seed is only valid
    // there, so fill it exactly when it is offered.
    if (scope === "user" || scope === "project") {
      await form.getByLabel("Seed memory").fill("Prefer table-driven tests.");
    }
    await form.getByLabel("Name").fill(name);
    await form.getByLabel("Description").fill("e2e memoryScope drift");
    await form.getByLabel("Content").fill(
      `---\nname: ${name}\ndescription: e2e memoryScope drift\n---\nReview Go code.`,
    );
    await page.getByRole("button", { name: /^create$/i }).click();

    await expect(
      page.getByText(name).first(),
      `the API rejected memoryScope="${scope}", which the subagent form offers`,
    ).toBeVisible({ timeout: 15_000 });
  }
});

test("every transport the Servers form offers is accepted by the real API", async ({ page }) => {
  await login(page, "boss", "boss");
  await gotoAdmin(page, "Servers");

  await page.getByRole("button", { name: /new server/i }).click();
  const form = page.locator("form");
  const transports = await optionValues(form, "Transport");

  expect(transports.length, "no transport options found — selector drifted?").toBeGreaterThan(1);
  for (const t of ["http", "sse"]) expect(transports).toContain(t);
  // stdio must NOT be offered: the remote gateway cannot dial a local stdio
  // subprocess, so the API rejects it with a 400. If it ever reappears in the
  // dropdown, the loop below would submit it and fail — but assert directly so
  // the reason is obvious rather than surfacing as a mystery API rejection.
  expect(transports, "stdio is not brokerable by the remote gateway").not.toContain("stdio");

  for (const transport of transports) {
    const name = `e2e-dd-transport-${transport}-${run}`;

    await page.getByRole("button", { name: /new server/i }).click();
    await form.getByLabel("Name").fill(name);
    await form.getByLabel("Description").fill(`e2e transport drift (${transport})`);
    await form.getByLabel("Transport").selectOption(transport);
    await form.getByLabel("Endpoint / command").fill("http://upstream:9000/mcp");
    await page.getByRole("button", { name: /^create$/i }).click();

    await expect(
      page.getByText(name).first(),
      `the API rejected transport="${transport}", which the Servers form offers`,
    ).toBeVisible({ timeout: 15_000 });
  }
});
