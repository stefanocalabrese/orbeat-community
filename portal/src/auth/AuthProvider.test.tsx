import type { ReactNode } from "react";
import { render, screen, fireEvent } from "@testing-library/react";
import { vi, test, expect, beforeEach } from "vitest";

// `react-oidc-context`'s real `AuthProvider` performs a redirect/discovery
// dance we can't drive in jsdom. Mock it to a passthrough and let each test
// control exactly what `useAuth()` (aliased `useOidc` in AuthProvider.tsx)
// returns, so we exercise the AppAuthProvider -> Bridge -> AuthCtx mapping
// (including the private decodeJwtPayload/rolesFrom helpers) without
// touching the network.
//
// `state` is declared via vi.hoisted because vi.mock factories are hoisted
// above imports — a plain `let` declared after this call would not yet be
// initialized when the factory closure captures it.
const state = vi.hoisted(() => ({ current: undefined as unknown }));

vi.mock("react-oidc-context", () => ({
  AuthProvider: ({ children }: { children: ReactNode }) => children,
  useAuth: () => state.current,
}));

// Imported after the mock is registered so AuthProvider.tsx picks it up.
import { AppAuthProvider } from "./AuthProvider";
import { useAuth } from "./useAuth";
import { notifyUnauthorized, setUnauthorizedHandler } from "../api/client";

function Consumer() {
  const auth = useAuth();
  return (
    <div>
      <div data-testid="authenticated">{String(auth.authenticated)}</div>
      <div data-testid="email">{auth.email}</div>
      <div data-testid="subject">{auth.subject}</div>
      <div data-testid="roles">{auth.roles.join(",")}</div>
      <button onClick={() => auth.login()}>login</button>
      <button onClick={() => auth.login("/admin/audit")}>login-with-target</button>
      <button onClick={auth.logout}>logout</button>
    </div>
  );
}

function b64url(payload: unknown): string {
  // Browser-only encoding (no Node `Buffer` — this file runs under tsc's
  // DOM-only lib) that mirrors decodeJwtPayload's own decode side: base64,
  // then swap to the URL-safe alphabet and drop padding.
  return btoa(JSON.stringify(payload))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

beforeEach(() => {
  sessionStorage.clear();
  // Reset so a future test that forgets to set state.current fails loudly
  // rather than silently inheriting the previous test's oidc session.
  state.current = undefined;
  // Reset the module-level 401 registry between tests.
  setUnauthorizedHandler(null);
});

test("maps an oidc session with profile realm_access into AuthCtx", () => {
  state.current = {
    isLoading: false,
    isAuthenticated: true,
    user: {
      access_token: "x",
      profile: {
        sub: "u1",
        email: "u@x",
        realm_access: { roles: ["orbeat-admin", "orbeat-user"] },
      },
    },
  };
  render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  expect(screen.getByTestId("authenticated")).toHaveTextContent("true");
  expect(screen.getByTestId("email")).toHaveTextContent("u@x");
  expect(screen.getByTestId("subject")).toHaveTextContent("u1");
  expect(screen.getByTestId("roles")).toHaveTextContent(
    "orbeat-admin,orbeat-user",
  );
});

test("falls back to decoding the access token when the profile has no realm_access", () => {
  // Real Keycloak access tokens carry realm_access in the JWT payload even
  // when the ID-token profile doesn't mirror it — this is the fallback path
  // through decodeJwtPayload.
  const jwt = `h.${b64url({ realm_access: { roles: ["orbeat-admin"] } })}.s`;
  state.current = {
    isLoading: false,
    isAuthenticated: true,
    user: { access_token: jwt, profile: { sub: "u2", email: "e2" } },
  };
  render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  expect(screen.getByTestId("roles")).toHaveTextContent("orbeat-admin");
});

test("a token whose payload segment isn't valid base64/JSON decodes to no roles", () => {
  // Exercises decodeJwtPayload's catch branch (not the `!part` early return):
  // two dots so `part` is truthy, but the middle segment contains characters
  // atob rejects outright.
  state.current = {
    isLoading: false,
    isAuthenticated: true,
    user: { access_token: "h.!!!.s", profile: { sub: "u3", email: "e3" } },
  };
  render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  expect(screen.getByTestId("roles")).toHaveTextContent("");
});

test("login() saves the current path as the post-login target and redirects", () => {
  const signinRedirect = vi.fn();
  state.current = {
    isLoading: false,
    isAuthenticated: false,
    user: undefined,
    signinRedirect,
  };
  window.history.pushState({}, "", "/connect");
  render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "login" }));
  expect(sessionStorage.getItem("orbeat.postLogin")).toBe("/connect");
  expect(signinRedirect).toHaveBeenCalledOnce();
});

test("login(target) persists the explicit deep-link target, not the current path", () => {
  // LoginPage forwards the guard-carried deep link (e.g. /admin/audit) as an
  // explicit argument; it must win over window.location.pathname, which at
  // that moment is /login.
  const signinRedirect = vi.fn();
  state.current = {
    isLoading: false,
    isAuthenticated: false,
    user: undefined,
    signinRedirect,
  };
  window.history.pushState({}, "", "/login");
  render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "login-with-target" }));
  expect(sessionStorage.getItem("orbeat.postLogin")).toBe("/admin/audit");
  expect(signinRedirect).toHaveBeenCalledOnce();
});

test("login() from /login defaults the post-login target to /catalog", () => {
  const signinRedirect = vi.fn();
  state.current = {
    isLoading: false,
    isAuthenticated: false,
    user: undefined,
    signinRedirect,
  };
  window.history.pushState({}, "", "/login");
  render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "login" }));
  expect(sessionStorage.getItem("orbeat.postLogin")).toBe("/catalog");
});

test("Bridge registers the 401 handler: notifyUnauthorized re-initiates login once, preserving the path", () => {
  const signinRedirect = vi.fn();
  state.current = {
    isLoading: false,
    isAuthenticated: true,
    user: { access_token: "t", profile: { sub: "s", email: "e" } },
    signinRedirect,
  };
  window.history.pushState({}, "", "/admin/servers");
  render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  notifyUnauthorized();
  notifyUnauthorized(); // a 401 burst (e.g. the 2s poll) must redirect exactly once
  expect(signinRedirect).toHaveBeenCalledOnce();
  expect(sessionStorage.getItem("orbeat.postLogin")).toBe("/admin/servers");
});

test("Bridge unregisters the 401 handler on unmount", () => {
  const signinRedirect = vi.fn();
  state.current = {
    isLoading: false,
    isAuthenticated: true,
    user: { access_token: "t", profile: { sub: "s", email: "e" } },
    signinRedirect,
  };
  const { unmount } = render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  unmount();
  notifyUnauthorized();
  expect(signinRedirect).not.toHaveBeenCalled();
});

test("logout() calls oidc.signoutRedirect", () => {
  const signoutRedirect = vi.fn();
  state.current = {
    isLoading: false,
    isAuthenticated: true,
    user: { access_token: "t", profile: { sub: "s", email: "e" } },
    signoutRedirect,
  };
  render(
    <AppAuthProvider>
      <Consumer />
    </AppAuthProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "logout" }));
  expect(signoutRedirect).toHaveBeenCalledOnce();
});
