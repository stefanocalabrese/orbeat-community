import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { vi, test, expect } from "vitest";
import { AuthCtx, type AuthState } from "../auth/useAuth";
import Layout from "./Layout";

function renderLayout(auth: AuthState) {
  return render(
    <AuthCtx.Provider value={auth}>
      <MemoryRouter>
        <Layout />
      </MemoryRouter>
    </AuthCtx.Provider>,
  );
}

const baseAuth: AuthState = {
  isLoading: false,
  authenticated: true,
  token: "t",
  subject: "boss",
  email: "boss@x",
  roles: ["orbeat-admin"],
  login: () => {},
  logout: () => {},
};

test("admin sees email, Catalog/Connect nav, and the Admin link", () => {
  renderLayout(baseAuth);

  expect(screen.getByText("boss@x")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Catalog" })).toHaveAttribute(
    "href",
    "/catalog",
  );
  expect(screen.getByRole("link", { name: "Connect" })).toHaveAttribute(
    "href",
    "/connect",
  );

  const adminLink = screen.getByRole("link", { name: "Admin" });
  expect(adminLink).toHaveAttribute("href", "/admin/servers");
});

test("non-admin does not see the Admin link", () => {
  renderLayout({ ...baseAuth, roles: [], email: "user@x" });

  expect(screen.getByText("user@x")).toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Admin" })).toBeNull();
});

test("clicking Sign out calls logout", async () => {
  const user = userEvent.setup();
  const logout = vi.fn();
  renderLayout({ ...baseAuth, logout });

  await user.click(screen.getByRole("button", { name: /sign out/i }));
  expect(logout).toHaveBeenCalledOnce();
});
