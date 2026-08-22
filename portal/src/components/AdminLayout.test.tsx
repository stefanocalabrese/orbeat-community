import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { test, expect } from "vitest";
import AdminLayout from "./AdminLayout";

function renderLayout() {
  return render(
    <MemoryRouter>
      <AdminLayout />
    </MemoryRouter>,
  );
}

test("renders the Console label and all nav links with correct hrefs", () => {
  renderLayout();

  expect(screen.getByText("Console")).toBeInTheDocument();

  const links: [string, string][] = [
    ["Servers", "/admin/servers"],
    ["Artifacts", "/admin/artifacts"],
    ["Review queue", "/admin/review"],
    ["Roles", "/admin/roles"],
    ["Entitlements", "/admin/entitlements"],
    ["Artifact entitlements", "/admin/artifact-entitlements"],
    ["Audit", "/admin/audit"],
  ];

  for (const [name, href] of links) {
    expect(screen.getByRole("link", { name })).toHaveAttribute("href", href);
  }
});

test("marks the current route's nav item active", () => {
  render(
    <MemoryRouter initialEntries={["/admin/roles"]}>
      <AdminLayout />
    </MemoryRouter>,
  );
  // NavLink stamps aria-current="page" on the active link — exercises the
  // isActive branch both ways (Roles active, Servers not).
  expect(screen.getByRole("link", { name: "Roles" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  expect(screen.getByRole("link", { name: "Servers" })).not.toHaveAttribute(
    "aria-current",
  );
});
