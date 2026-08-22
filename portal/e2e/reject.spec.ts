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

// Per-run unique (all-digits suffix -> slug-safe), evaluated once at module
// load and shared across this file's serial tests — see approval.spec.ts's
// ARTIFACT_NAME comment: a fixed name 409s against a leftover row from a
// prior run against a surviving stack.
const ARTIFACT_NAME = `e2e-reject-skill-${Date.now()}`;

// fable-audit §7 #17: POST /v1/admin/artifacts/{id}/reject now requires a
// non-blank reason (400 "rejection reason is required" otherwise). The Go
// handler tests call handleRejectArtifact directly (never crossing HTTP,
// routing, or CORS) and the portal unit tests mock fetch (green even with a
// dead feature — see the v1.16.0 mcp_server.status lesson this repo already
// paid for once). This spec is the one gate that exercises the real browser
// against the real running api.
//
// The two realm admins are "boss" and "boss2" (approval.spec.ts's two-party
// pattern) — the realm has no "alice"-as-admin or "bob" user, so this reuses
// the same pair approval.spec.ts already established for the author/reviewer
// split, rather than inventing new realm users.
test.describe.configure({ mode: "serial" });

test("boss authors a skill and submits it for review", async ({ page }) => {
  await login(page, "boss", "boss");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Artifacts" }).click();

  await page.getByRole("button", { name: /new artifact/i }).click();
  // Scope field lookups to the open <form>: row-action buttons carry aria-labels
  // like "Edit <name>" and getByLabel matches case-insensitive substrings, so an
  // unscoped field lookup can also resolve to those table buttons.
  const form = page.locator("form");
  await form.getByLabel(/name/i).fill(ARTIFACT_NAME);
  await form.getByLabel(/description/i).fill("e2e required-reject-reason flow skill");
  await form.getByLabel("Visibility").selectOption("org");
  await form.getByLabel(/content/i).fill(
    `---\nname: ${ARTIFACT_NAME}\ndescription: e2e required-reject-reason flow skill\n---\nRun gofmt on all Go files.`,
  );
  await page.getByRole("button", { name: /^create$/i }).click();

  const row = page.locator("tr", { hasText: ARTIFACT_NAME });
  await expect(row).toBeVisible();
  await expect(row.getByText("draft", { exact: true })).toBeVisible();

  await row.getByRole("button", { name: /^submit\b/i }).click();
  await expect(row.getByText("pending", { exact: true })).toBeVisible();
});

test("boss2 cannot reject with a blank reason, and the artifact survives the attempt", async ({ page }) => {
  await login(page, "boss2", "boss2");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Review queue" }).click();
  await expect(page.getByRole("heading", { name: /review queue/i })).toBeVisible();

  const card = page.locator(".rounded-xl", { hasText: ARTIFACT_NAME });
  await expect(card).toBeVisible();

  // Reject box starts empty. There is deliberately no client-side required-
  // field guard (the button stays enabled) — the rule is server-side only.
  await card.getByRole("button", { name: /^reject$/i }).click();
  await expect(card.getByText("rejection reason is required")).toBeVisible();

  // THE POINT OF THIS TEST: an inline error message alone is not proof the
  // server refused the mutation — a server that rejects the artifact AND
  // returns an error would pass an assertion that only checks the message.
  // Reload from scratch and confirm the artifact is still pending via two
  // independent reads: it still surfaces in the (pending-only) review queue,
  // and its own row still carries the "pending" state badge, not "rejected".
  await page.reload();
  await expect(page.getByRole("heading", { name: /review queue/i })).toBeVisible();
  await expect(page.locator(".rounded-xl", { hasText: ARTIFACT_NAME })).toBeVisible();

  await page.getByRole("link", { name: "Artifacts" }).click();
  const row = page.locator("tr", { hasText: ARTIFACT_NAME });
  await expect(row.getByText("pending", { exact: true })).toBeVisible();
  await expect(row.getByText("rejected", { exact: true })).toHaveCount(0);
});

test("boss2 rejects with a reason, and the artifact moves to rejected", async ({ page }) => {
  await login(page, "boss2", "boss2");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("link", { name: "Review queue" }).click();
  const card = page.locator(".rounded-xl", { hasText: ARTIFACT_NAME });
  await expect(card).toBeVisible();

  await card.getByLabel("Reject reason").fill("does not meet the coding standard");
  await card.getByRole("button", { name: /^reject$/i }).click();

  // The card leaves the (pending-only) review queue once rejected.
  await expect(page.locator(".rounded-xl", { hasText: ARTIFACT_NAME })).toHaveCount(0);

  await page.getByRole("link", { name: "Artifacts" }).click();
  const row = page.locator("tr", { hasText: ARTIFACT_NAME });
  await expect(row.getByText("rejected", { exact: true })).toBeVisible();
});
