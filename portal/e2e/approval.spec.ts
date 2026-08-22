import { expect, test, type Page } from "@playwright/test";

async function login(page: Page, user: string, pass: string) {
  await page.goto("/");
  await page.getByRole("button", { name: /sign in/i }).click();
  // Keycloak login page (external origin, localhost:8088)
  await page.fill("#username", user);
  await page.fill("#password", pass);
  await page.click("#kc-login");
  // Wait for the OIDC callback to complete and the app to settle on /catalog.
  // oidc-client-ts processes the code and calls onSigninCallback which does
  // history.replaceState to the post-login target. We wait for the URL to
  // stabilise on /catalog (not /auth/callback) before asserting the heading.
  await page.waitForURL(/\/catalog/, { timeout: 30_000 });
  await expect(page.getByRole("heading", { name: /^catalog$/i })).toBeVisible({ timeout: 15_000 });
}

// Per-run unique (all-digits suffix → slug-safe), evaluated once at module
// load and shared across this file's serial tests. A fixed name made a second
// run against a surviving stack fail: the create 409s on the leftover artifact
// from the prior run (the servers.spec.ts lesson, on the approval surface).
const ARTIFACT_NAME = `e2e-approval-skill-${Date.now()}`;

// Two-admin governance flow (P4 artifact approval): boss authors + submits,
// boss2 (a distinct admin) reviews + approves — approving your own submission
// is rejected server-side, so this genuinely exercises the two-party check.
// Each Playwright test gets a fresh browser context (no shared cookies), so
// moving from the "boss" test to the "boss2" test is itself the log-out step;
// there is no need to click the app's own "Sign out" button in between.
test.describe.configure({ mode: "serial" });

test("boss authors an org skill and submits it for review", async ({ page }) => {
  await login(page, "boss", "boss");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Artifacts" }).click();

  await page.getByRole("button", { name: /new artifact/i }).click();
  // Scope field lookups to the open <form>: row-action buttons carry aria-labels
  // like "Edit <name>" and getByLabel matches case-insensitive substrings, so an
  // unscoped field lookup can also resolve to those table buttons. The form is a
  // <form>; the row buttons live in the <table> outside it.
  const form = page.locator("form");
  await form.getByLabel(/name/i).fill(ARTIFACT_NAME);
  await form.getByLabel(/description/i).fill("e2e two-admin approval flow skill");
  await form.getByLabel("Visibility").selectOption("org");
  await form.getByLabel(/content/i).fill(
    `---\nname: ${ARTIFACT_NAME}\ndescription: e2e two-admin approval flow skill\n---\nRun gofmt on all Go files.`,
  );
  await page.getByRole("button", { name: /^create$/i }).click();

  const row = page.locator("tr", { hasText: ARTIFACT_NAME });
  await expect(row).toBeVisible();
  await expect(row.getByText("draft", { exact: true })).toBeVisible();

  await row.getByRole("button", { name: /^submit\b/i }).click();
  await expect(row.getByText("pending", { exact: true })).toBeVisible();
});

test("boss2 approves boss's submission from the review queue", async ({ page }) => {
  await login(page, "boss2", "boss2");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Review queue" }).click();
  await expect(page.getByRole("heading", { name: /review queue/i })).toBeVisible();

  const card = page.locator(".rounded-xl", { hasText: ARTIFACT_NAME });
  await expect(card).toBeVisible();
  await card.getByRole("button", { name: /^approve$/i }).click();

  // The card leaves the (pending-only) review queue once approved.
  await expect(page.locator(".rounded-xl", { hasText: ARTIFACT_NAME })).toHaveCount(0);

  await page.getByRole("link", { name: "Artifacts" }).click();
  const row = page.locator("tr", { hasText: ARTIFACT_NAME });
  await expect(row.getByText("approved", { exact: true })).toBeVisible();
});

// Revision history + rollback. The prior two tests left ARTIFACT_NAME approved at
// revision 1. boss edits it to v2 and submits; boss2 (distinct approver, so the
// separation-of-duties check is satisfied) approves v2, then rolls distribution
// back to revision 1 — a single-admin, audited action.
test("boss edits the skill to v2 and submits it", async ({ page }) => {
  await login(page, "boss", "boss");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Artifacts" }).click();

  const row = page.locator("tr", { hasText: ARTIFACT_NAME });
  await expect(row).toBeVisible();
  // Row-action buttons gained aria-labels ("Edit <name>"), so their accessible
  // name is now the label, not the bare verb — anchor on the leading verb
  // (start-anchored, so a name embedding "edit" in another button can't match).
  await row.getByRole("button", { name: /^edit\b/i }).click();

  const form = page.locator("form");
  await form.getByLabel(/content/i).fill(
    `---\nname: ${ARTIFACT_NAME}\ndescription: e2e two-admin approval flow skill\n---\nRun gofmt AND go vet.`,
  );
  await page.getByRole("button", { name: /^save$/i }).click();

  // Editing an approved artifact returns the working copy to draft; submit it again.
  await row.getByRole("button", { name: /^submit\b/i }).click();
  await expect(row.getByText("pending", { exact: true })).toBeVisible();
});

test("boss2 approves v2, then rolls distribution back to v1", async ({ page }) => {
  await login(page, "boss2", "boss2");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Review queue" }).click();
  const card = page.locator(".rounded-xl", { hasText: ARTIFACT_NAME });
  await expect(card).toBeVisible();
  await card.getByRole("button", { name: /^approve$/i }).click();
  await expect(page.locator(".rounded-xl", { hasText: ARTIFACT_NAME })).toHaveCount(0);

  // Open the artifact's version history: two approved revisions, #2 current.
  await page.getByRole("link", { name: "Artifacts" }).click();
  const row = page.locator("tr", { hasText: ARTIFACT_NAME });
  await row.getByRole("button", { name: /^history\b/i }).click();
  await expect(page.getByText("#2")).toBeVisible();
  await expect(page.getByText("current").first()).toBeVisible();

  // Roll back to #1 (confirm dialog). A rollback revision (#3) is appended + current.
  page.once("dialog", (d) => d.accept());
  await page.getByRole("button", { name: /roll back to #1/i }).click();
  await expect(page.getByText("rollback").first()).toBeVisible();
});
