export interface AppConfig {
  apiBase: string;
  gatewayUrl: string;
  marketplaceSource: string;
  oidcAuthority: string;
  oidcClientId: string;
}

// Build-time fallbacks. Used only when /config.json is unavailable — i.e. the
// `npm run dev` Vite server (which doesn't serve it). In the containerized
// portal the Go server serves /config.json from ORBEAT_PORTAL_* env, so these
// values are overwritten at boot by loadConfig().
const fallback: AppConfig = {
  apiBase: import.meta.env.VITE_API_BASE ?? "http://localhost:8080",
  gatewayUrl: import.meta.env.VITE_GATEWAY_URL ?? "http://localhost:8090",
  marketplaceSource: import.meta.env.VITE_MARKETPLACE_SOURCE ?? "./marketplace",
  oidcAuthority: import.meta.env.VITE_OIDC_AUTHORITY ?? "http://localhost:8088/realms/orbeat",
  oidcClientId: import.meta.env.VITE_OIDC_CLIENT_ID ?? "orbeat-portal",
};

// A mutable singleton, seeded with the fallback and overwritten in place by
// loadConfig() before the app mounts. Callers read config.<field> at call/render
// time (never captured at module-eval time — see AuthProvider).
export const config: AppConfig = { ...fallback };

const KEYS: (keyof AppConfig)[] = [
  "apiBase",
  "gatewayUrl",
  "marketplaceSource",
  "oidcAuthority",
  "oidcClientId",
];

// Fetch runtime config from the portal server and overwrite `config` in place.
// Any failure (dev Vite server 404, non-JSON body, network error) leaves the
// fallback intact — a broken /config.json degrades to the baked defaults rather
// than throwing during boot.
export async function loadConfig(): Promise<void> {
  try {
    // Bound this fetch: it gates the entire app render (main.tsx), so a hung
    // /config.json must not leave a blank screen forever — a timeout routes
    // through the catch below and the app boots on the fallback. Mirrors the
    // 30s ceiling apiFetch puts on every other request.
    const res = await fetch("/config.json", { cache: "no-store", signal: AbortSignal.timeout(5000) });
    if (!res.ok) return;
    const data = (await res.json()) as Partial<Record<keyof AppConfig, unknown>>;
    for (const k of KEYS) {
      const v = data[k];
      if (typeof v === "string" && v !== "") config[k] = v;
    }
  } catch {
    // keep fallback
  }
}
