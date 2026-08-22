import { render, screen, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { vi, test, expect } from "vitest";
import { AuthCtx } from "../auth/useAuth";
import LoginPage from "./LoginPage";

const anon = {
  isLoading: false,
  authenticated: false,
  token: "",
  subject: "",
  email: "",
  roles: [] as string[],
  logout: () => {},
};

function renderLogin(login: (target?: string) => void, state?: unknown) {
  return render(
    <AuthCtx.Provider value={{ ...anon, login }}>
      <MemoryRouter initialEntries={[{ pathname: "/login", state }]}>
        <LoginPage />
      </MemoryRouter>
    </AuthCtx.Provider>,
  );
}

// The guard redirects to /login with the intended URL in state.from; signing in
// must forward that target into login() so the deep link survives SSO. An
// unauthenticated visit to /admin/audit therefore ends up back at /admin/audit:
// the guard puts it in state.from (guards.test.tsx), LoginPage forwards it here,
// and login(target) persists it as the post-login restore target
// (AuthProvider.test.tsx).
test("forwards the guard-provided deep-link target into login()", () => {
  const login = vi.fn();
  renderLogin(login, { from: { pathname: "/admin/audit" } });
  fireEvent.click(screen.getByRole("button", { name: /sign in/i }));
  expect(login).toHaveBeenCalledWith("/admin/audit");
});

test("without a guard-provided target, login() is called with no target", () => {
  const login = vi.fn();
  renderLogin(login);
  fireEvent.click(screen.getByRole("button", { name: /sign in/i }));
  expect(login).toHaveBeenCalledWith(undefined);
});
