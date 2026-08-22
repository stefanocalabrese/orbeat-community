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
});
