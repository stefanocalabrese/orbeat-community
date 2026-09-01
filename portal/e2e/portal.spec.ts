import { expect, test, type Page } from "@playwright/test";

const API_BASE = "http://localhost:8080";

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

test.describe.configure({ mode: "serial" }); // boss seeds state; alice then sees it

test("boss creates server, role, entitlement via admin UI", async ({ page }) => {
  await login(page, "boss", "boss");

  // Scope FORM-FIELD lookups to the open <form>. Row-action buttons carry
  // aria-labels like "Edit <name>"/"Delete <name>", and getByLabel matches
  // case-insensitive substrings — so a leftover row whose name embeds a field
  // word would make an unscoped getByLabel resolve to those buttons too
  // (strict-mode violation). Each admin page shows exactly one form when open,
  // and the row buttons live in a <table> outside it, so this disambiguates.
  const form = page.locator("form");

  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("button", { name: /new server/i }).click();
  await form.getByLabel(/name/i).fill("e2e-upstream");
  await form.getByLabel(/endpoint/i).fill("http://upstream:9000/mcp");
  // Captured (the concurrency.spec.ts/roles.spec.ts idiom: registered before
  // the click, read from the BROWSER's own request) so the audit check below
  // can prove THIS create's own event exists, not merely that some
  // server.create row survives.
  const createServerResponse = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url() === `${API_BASE}/v1/admin/servers`,
  );
  await page.getByRole("button", { name: /^create$/i }).click();
  await expect(page.getByRole("cell", { name: "e2e-upstream", exact: true })).toBeVisible();
  const createdServerId = ((await (await createServerResponse).json()) as { id: string }).id;

  // Create the role that the entitlement will reference.
  // orbeat roles are stored in Postgres and must match the IdP realm roles.
  await page.getByRole("link", { name: "Roles" }).click();
  await page.getByRole("textbox", { name: /role name/i }).fill("orbeat-user");
  await page.getByRole("button", { name: /^create$/i }).click();
  await expect(page.getByRole("listitem").filter({ hasText: "orbeat-user" })).toBeVisible();

  await page.getByRole("link", { name: "Entitlements", exact: true }).click();
  await page.getByRole("button", { name: /new entitlement/i }).click();
  await form.getByLabel(/allowed tools/i).fill("echo");
  // Select the role EXPLICITLY. EntitlementsPage falls back to roles.rows[0]
  // when the select is untouched, and ListRolesPage orders by (name, id) — so
  // relying on the default silently binds this entitlement to whichever role
  // happens to sort first. Any concurrently-running spec that creates a role
  // sorting before "orbeat-user" (roles.spec.ts's `e2erole<ts>-role` does)
  // steals this grant: alice's catalog then comes up empty here, and the other
  // spec's role gains an entitlement it never asked for. Both failures observed
  // together; naming around it would only re-arm the trap for the next spec.
  await form.getByLabel("Role", { exact: true }).selectOption({ label: "orbeat-user" });
  await form.getByLabel(/server/i).selectOption({ label: "e2e-upstream" });
  await page.getByRole("button", { name: /^create$/i }).click();
  await expect(page.getByRole("cell", { name: "e2e-upstream", exact: true })).toBeVisible();

  await page.getByRole("link", { name: "Audit" }).click();
  // The audit log is one shared, size-capped (default 100 rows per page)
  // table for the whole e2e run: pagination.spec.ts alone writes 105+
  // server.create events in well under a second when it runs concurrently.
  // Filtering by action is the pattern `9a92728` established for
  // audit-filters.spec.ts: it narrows the competition to other server.create
  // rows only. That narrowing alone is NOT enough here the way it is for
  // audit-filters.spec.ts's role.create check, role.create is written by
  // only two specs, but server.create is exactly what pagination.spec.ts
  // floods, so even the filtered first page can push this test's own row
  // off (reproduced live: a full-suite run at 9 workers failed here with
  // the row genuinely absent from the filtered page-1 response, not merely
  // hidden by an unfiltered one). So on top of the action filter, this walks
  // "Load more" (AuditPage.tsx's own pagination, bounded by its own
  // exhaustion rather than a clock) until the CAPTURED server id's row is
  // found, proving it is THIS test's own row rather than merely some other
  // spec's server.create surviving in its place.
  await page.getByLabel("Filter by action").fill("server.create");
  await page.getByRole("button", { name: /apply filters/i }).click();
  const auditRow = page.getByRole("cell", { name: createdServerId, exact: true });
  const auditLoadMore = page.getByRole("button", { name: "Load more", exact: true });
  for (
    let i = 0;
    i < 50 &&
    !(await auditRow
      .waitFor({ state: "visible", timeout: 2_000 })
      .then(() => true)
      .catch(() => false));
    i++
  ) {
    if (!(await auditLoadMore.isVisible().catch(() => false))) break;
    const before = await page.locator("tbody tr").count();
    await auditLoadMore.click();
    await expect.poll(() => page.locator("tbody tr").count(), { timeout: 15_000 }).toBeGreaterThan(before);
  }
  await expect(auditRow, `audit event for created server ${createdServerId} never appeared`).toBeVisible({
    timeout: 5_000,
  });

  // ── Artifacts: create a skill and assert publish status ──────────────────
  await page.getByRole("link", { name: "Artifacts" }).click();
  await page.getByRole("button", { name: /new artifact/i }).click();
  await form.getByLabel(/name/i).fill("e2e-skill");
  // type defaults to skill; description and content are required
  await form.getByLabel(/description/i).fill("e2e smoke skill");
  await form.getByLabel(/content/i).fill(
    "---\nname: e2e-skill\ndescription: e2e smoke skill\n---\nRun gofmt on all Go files.",
  );
  await page.getByRole("button", { name: /^create$/i }).click();
  // The skill should appear in the list
  await expect(page.getByRole("cell", { name: "e2e-skill", exact: true })).toBeVisible();

  // The publish-status banner should eventually show a non-empty commit hash.
  // The async worker debounces ~750ms — allow up to 30s for CI.
  await expect(
    page.getByText(/[0-9a-f]{7,}/i).first(),
  ).toBeVisible({ timeout: 30_000 });

  // ── Per-role artifact: create a role-visibility artifact and entitle it ───
  // (Channel-2 / orbeat-sync path: role artifacts ship only to entitled roles.)
  await page.getByRole("button", { name: /new artifact/i }).click();
  await form.getByLabel(/name/i).fill("e2e-role-skill");
  await form.getByLabel(/description/i).fill("e2e role-only skill");
  await form.getByLabel("Visibility").selectOption("role");
  await form.getByLabel(/content/i).fill(
    "---\nname: e2e-role-skill\ndescription: e2e role-only skill\n---\nRole-scoped skill body.",
  );
  await page.getByRole("button", { name: /^create$/i }).click();
  await expect(page.getByRole("cell", { name: "e2e-role-skill", exact: true })).toBeVisible();

  // Grant the role-visibility artifact to the orbeat-user role on the new
  // Artifact-entitlements admin page. The role select defaults to orbeat-user
  // (only role) and the artifact select to e2e-role-skill (only role artifact).
  await page.getByRole("link", { name: "Artifact entitlements" }).click();
  await page.getByRole("button", { name: /new entitlement/i }).click();
  // Select the role + artifact explicitly instead of trusting the form's default
  // selection. Other role-visibility artifacts share the stack (e.g.
  // dropdowns.spec.ts's e2e-dd-*-role, created concurrently in another worker),
  // so "e2e-role-skill is the only role artifact" is not a safe assumption — the
  // unset default would otherwise entitle roleArtifacts[0], the wrong one.
  await form.getByLabel("Role").selectOption({ label: "orbeat-user" });
  await form.getByLabel("Artifact").selectOption({ label: "e2e-role-skill" });
  await page.getByRole("button", { name: /^create$/i }).click();
  // Assert the new entitlement as a pairing within its own row: other artifact
  // entitlements can share the orbeat-user role, so an unscoped "orbeat-user"
  // cell may resolve to several rows.
  const roleEntRow = page.locator("tr", { hasText: "e2e-role-skill" });
  await expect(roleEntRow.getByRole("cell", { name: "e2e-role-skill", exact: true })).toBeVisible();
  await expect(roleEntRow.getByRole("cell", { name: "orbeat-user", exact: true })).toBeVisible();
});

test("alice sees entitled server, connect page; no admin nav; /admin bounces", async ({ page }) => {
  await login(page, "alice", "alice");
  await expect(page.getByText("e2e-upstream")).toBeVisible();
  await expect(page.getByRole("link", { name: "Admin" })).toHaveCount(0);

  await page.getByRole("navigation").getByRole("link", { name: "Connect" }).click();
  await expect(page.getByText(/localhost:8090\/mcp/).first()).toBeVisible();
  await expect(page.getByText(/claude mcp add/)).toBeVisible();
  await expect(page.getByText(/claude plugin marketplace add/).first()).toBeVisible();
  await expect(page.getByText(/claude plugin install orbeat-gateway@orbeat/).first()).toBeVisible();
  await expect(page.getByText(/echo/).first()).toBeVisible();

  await page.goto("/admin/servers");
  await expect(page).toHaveURL(/\/catalog/); // RequireAdmin bounce
});
