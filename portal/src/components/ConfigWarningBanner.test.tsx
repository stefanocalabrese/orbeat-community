/**
 * B16: loadConfig() degrading to the baked-in fallback used to be silent —
 * an operator who forgot ORBEAT_PORTAL_API_BASE (or a proxy returning HTML
 * for /config.json) got an admin console pointed at http://localhost:8080
 * with zero on-screen signal. This banner is that signal.
 *
 * Uses the same dynamic-import-per-test pattern as config.test.ts (rather
 * than reassigning the module's exported `configLoadFailed` binding, which
 * ESM makes read-only from a consumer) so `configLoadFailed` is driven
 * through the real loadConfig() + a mocked fetch, not poked directly.
 */
import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetModules();
});

async function renderBanner(enabled: boolean, fetchImpl: () => Promise<Response>) {
  vi.stubGlobal("fetch", vi.fn(fetchImpl));
  const configMod = await import("../config");
  await configMod.loadConfig();
  const { default: ConfigWarningBanner } = await import("./ConfigWarningBanner");
  return render(<ConfigWarningBanner enabled={enabled} />);
}

const notFound = async () => new Response("not found", { status: 404 });
const okConfig = async () =>
  new Response(JSON.stringify({ apiBase: "https://orbeat.example.com" }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

describe("ConfigWarningBanner", () => {
  it("renders a persistent alert when the runtime config failed to load and the banner is enabled", async () => {
    await renderBanner(true, notFound);
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(/runtime configuration failed to load/i);
  });

  it("names the fallback the app is actually running on, not a generic message", async () => {
    await renderBanner(true, notFound);
    // The fallback in config.ts's own default is http://localhost:8080 —
    // asserting the literal value (not just "a URL") is what would catch a
    // banner that renders a static, unhelpful string instead of reading
    // config.apiBase.
    expect(screen.getByRole("alert")).toHaveTextContent(/localhost:8080/);
  });

  it("renders nothing when the banner is disabled (the dev server, where /config.json 404ing is expected)", async () => {
    await renderBanner(false, notFound);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders nothing when the runtime config loaded successfully", async () => {
    await renderBanner(true, okConfig);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
