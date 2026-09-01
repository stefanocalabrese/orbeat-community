import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetModules();
});

describe("loadConfig", () => {
  it("populates config from /config.json", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            apiBase: "https://orbeat.example.com",
            gatewayUrl: "https://orbeat.example.com/mcp",
            oidcAuthority: "https://auth.orbeat.example.com/realms/orbeat",
            oidcClientId: "orbeat-portal",
            marketplaceSource: "./marketplace",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    const mod = await import("./config");
    await mod.loadConfig();
    expect(mod.config.apiBase).toBe("https://orbeat.example.com");
    expect(mod.config.oidcAuthority).toBe("https://auth.orbeat.example.com/realms/orbeat");
  });

  it("keeps the fallback when /config.json is unavailable", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("not found", { status: 404 })));
    const mod = await import("./config");
    const fallbackApiBase = mod.config.apiBase;
    await mod.loadConfig();
    expect(mod.config.apiBase).toBe(fallbackApiBase);
  });

  it("keeps the fallback when the body is not JSON (dev Vite server)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("<!doctype html>", { status: 200 })),
    );
    const mod = await import("./config");
    const fallbackApiBase = mod.config.apiBase;
    await mod.loadConfig();
    expect(mod.config.apiBase).toBe(fallbackApiBase);
  });

  // B16: a failed load must be OBSERVABLE, not just silently absorbed --
  // otherwise an operator who forgot ORBEAT_PORTAL_API_BASE (or a proxy
  // returning HTML for /config.json) gets an admin console silently aimed at
  // http://localhost:8080 with no way to tell it happened short of opening
  // devtools. `configLoadFailed` is the flag ConfigWarningBanner reads.
  it("flags configLoadFailed on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("not found", { status: 404 })));
    const mod = await import("./config");
    expect(mod.configLoadFailed).toBe(false);
    await mod.loadConfig();
    expect(mod.configLoadFailed).toBe(true);
  });

  it("flags configLoadFailed on a non-JSON body", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("<!doctype html>", { status: 200 })),
    );
    const mod = await import("./config");
    await mod.loadConfig();
    expect(mod.configLoadFailed).toBe(true);
  });

  it("flags configLoadFailed on a network error (fetch rejects)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("Failed to fetch");
      }),
    );
    const mod = await import("./config");
    await mod.loadConfig();
    expect(mod.configLoadFailed).toBe(true);
  });

  it("does NOT flag configLoadFailed on a successful load", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(JSON.stringify({ apiBase: "https://orbeat.example.com" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    const mod = await import("./config");
    await mod.loadConfig();
    expect(mod.configLoadFailed).toBe(false);
  });

  it("a later successful load clears a prior failure", async () => {
    const mod = await import("./config");
    vi.stubGlobal("fetch", vi.fn(async () => new Response("not found", { status: 404 })));
    await mod.loadConfig();
    expect(mod.configLoadFailed).toBe(true);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(JSON.stringify({ apiBase: "https://orbeat.example.com" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    await mod.loadConfig();
    expect(mod.configLoadFailed).toBe(false);
  });
});
