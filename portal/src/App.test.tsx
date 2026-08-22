import { render, screen, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { vi, test, expect, afterEach } from "vitest";
import { AuthCtx } from "./auth/useAuth";
import type { AuthState } from "./auth/useAuth";
import { createAppQueryClient } from "./api/client";
import App from "./App";

// App owns its own <BrowserRouter>, which reads the real `window.location` —
// unlike the page-level tests (which supply their own <MemoryRouter>), the
// route under test is set by pushing history state before each render.

const unauthenticated: AuthState = {
  isLoading: false,
  authenticated: false,
  token: "",
  subject: "",
  email: "",
  roles: [],
  login: () => {},
  logout: () => {},
};

const alice: AuthState = {
  isLoading: false,
  authenticated: true,
  token: "t",
  subject: "alice",
  email: "a@x",
  roles: ["orbeat-user"],
  login: () => {},
  logout: () => {},
};

function renderApp(auth: AuthState) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <AuthCtx.Provider value={auth}>
      <QueryClientProvider client={qc}>
        <App />
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

// No setLimitReachedHandler(null) here: cleanup() unmounts App, which unmounts
// LimitReachedGate, whose own effect cleanup clears the handler.
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("unauthenticated at /login renders LoginPage", () => {
  window.history.pushState({}, "", "/login");
  renderApp(unauthenticated);
  expect(screen.getByRole("heading", { name: /orbeat/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
});

test("unauthenticated at a guarded route is redirected to LoginPage", () => {
  window.history.pushState({}, "", "/catalog");
  renderApp(unauthenticated);
  expect(screen.getByRole("heading", { name: /orbeat/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
});

test("authenticated at /catalog routes through RequireAuth + Layout into CatalogPage", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        servers: [
          {
            id: "1",
            name: "github",
            description: "GH tools",
            transport: "http",
            version: "",
            protocolVersion: "",
            status: "active",
            allowedTools: ["create_issue"],
          },
        ],
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    ),
  );
  window.history.pushState({}, "", "/catalog");
  renderApp(alice);
  expect(await screen.findByText("github")).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: /catalog/i })).toBeInTheDocument();
});

/**
 * THE WIRING GATE. Every other test for the Community cap dialog drives the
 * pieces directly: the modal with a hand-made payload, the gate with a direct
 * notifyLimitReached, apiFetch with a mocked Response. None of them can tell
 * whether <LimitReachedGate /> is actually mounted in the running app, so
 * deleting BOTH its import and its element from App.tsx left the whole suite
 * at 196/196 with the feature wired to nothing (`noUnusedLocals` catches
 * deleting only the element, never both). That is the shape that shipped in
 * v1.25.0, where the gateway's SSRF dial guard went out wired to nothing while
 * its package stayed green.
 *
 * This is the only test that fails for that. It uses the REAL
 * createAppQueryClient rather than the bare QueryClient the tests above build,
 * because the 402 notification travels through that client's QueryCache
 * onError: a plain QueryClient would never fire it, and the test would pass
 * with the whole feature removed.
 *
 * `current` (11) deliberately EXCEEDS `max` (10), the shape an install already
 * over its cap really sends: checkServerActiveCap (internal/api/caps.go) fires
 * on `current >= max` and reports the true count, so an Enterprise tree
 * regenerated as Community reports more than the cap. A fixture with
 * `max === current` cannot tell the two numbers apart, and would pass with
 * `{info.max}` rendered in the `current` slot.
 */
test("a 402 from a page query opens the cap dialog, proving the gate is mounted in App", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        error: { message: "community edition limit reached: seats (11 of 10 used)" },
        limit: { resource: "seats", max: 10, current: 11, contact: "sales@example.test" },
      }),
      { status: 402, headers: { "Content-Type": "application/json" } },
    ),
  );
  window.history.pushState({}, "", "/catalog");
  render(
    <AuthCtx.Provider value={alice}>
      <QueryClientProvider client={createAppQueryClient()}>
        <App />
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
  const dialog = await screen.findByRole("dialog", { name: "Free edition limit reached" });
  expect(dialog.textContent).toContain("seats: 11 of 10 used");
  expect(screen.getByRole("link", { name: "sales@example.test" })).toHaveAttribute(
    "href",
    "mailto:sales@example.test",
  );
});
