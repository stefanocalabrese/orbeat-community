import type React from "react";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Routes, Route, useLocation } from "react-router";
import { RequireAuth, RequireAdmin } from "./guards";
import { AuthCtx } from "./useAuth";

// Probe standing in for LoginPage: renders the deep-link target the guard
// carried in navigation state, so tests can assert it survived the redirect.
function LoginProbe() {
  const location = useLocation();
  const from = (location.state as { from?: { pathname?: string } } | null)?.from?.pathname;
  return (
    <div>
      <div>login-page</div>
      <div data-testid="from">{from ?? "none"}</div>
    </div>
  );
}

function withAuth(ui: React.ReactNode, value: React.ContextType<typeof AuthCtx>) {
  return render(
    <AuthCtx.Provider value={value}>
      <MemoryRouter initialEntries={["/protected"]}>
        <Routes>
          <Route path="/protected" element={ui} />
          <Route path="/login" element={<LoginProbe />} />
          <Route path="/catalog" element={<div>catalog-page</div>} />
        </Routes>
      </MemoryRouter>
    </AuthCtx.Provider>,
  );
}

const alice = { isLoading: false, authenticated: true, roles: ["orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "alice", email: "a@x" };
const boss = { ...alice, roles: ["orbeat-user", "orbeat-admin"], subject: "boss" };
const anon = { ...alice, authenticated: false, token: "" };

test("RequireAuth renders children when authenticated", () => {
  withAuth(<RequireAuth><div>secret</div></RequireAuth>, alice);
  expect(screen.getByText("secret")).toBeInTheDocument();
});

test("RequireAuth redirects anonymous to /login", () => {
  withAuth(<RequireAuth><div>secret</div></RequireAuth>, anon);
  expect(screen.getByText("login-page")).toBeInTheDocument();
});

test("RequireAdmin bounces non-admin to /catalog", () => {
  withAuth(<RequireAdmin><div>admin-stuff</div></RequireAdmin>, alice);
  expect(screen.getByText("catalog-page")).toBeInTheDocument();
});

test("RequireAdmin renders for admin", () => {
  withAuth(<RequireAdmin><div>admin-stuff</div></RequireAdmin>, boss);
  expect(screen.getByText("admin-stuff")).toBeInTheDocument();
});

// Deep links must survive the login round-trip: the guard carries the intended
// URL in navigation state so LoginPage can restore it after SSO.
test("RequireAuth passes the intended location to /login in state.from", () => {
  withAuth(<RequireAuth><div>secret</div></RequireAuth>, anon);
  expect(screen.getByTestId("from")).toHaveTextContent("/protected");
});

test("RequireAdmin passes the intended location to /login in state.from", () => {
  withAuth(<RequireAdmin><div>admin-stuff</div></RequireAdmin>, anon);
  expect(screen.getByTestId("from")).toHaveTextContent("/protected");
});
