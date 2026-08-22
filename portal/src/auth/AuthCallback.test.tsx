import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import AuthCallback from "./AuthCallback";

// Control what react-oidc-context reports without a real OidcProvider.
const oidc = vi.hoisted(() => ({ error: undefined as Error | undefined }));
vi.mock("react-oidc-context", () => ({
  useAuth: () => ({ error: oidc.error, isLoading: false, isAuthenticated: false }),
}));

function renderAt(search: string) {
  // AuthCallback reads the REAL window.location.search (that's where the IdP
  // puts code/state) — the MemoryRouter only receives the recovery Navigate.
  window.history.replaceState({}, "", `/auth/callback${search}`);
  return render(
    <MemoryRouter initialEntries={["/auth/callback"]}>
      <Routes>
        <Route path="/auth/callback" element={<AuthCallback />} />
        <Route path="/catalog" element={<div>CATALOG</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  oidc.error = undefined;
});
afterEach(() => {
  vi.restoreAllMocks();
  window.history.replaceState({}, "", "/");
});

test("a stale visit with no code/state redirects to /catalog (not a blank page)", () => {
  renderAt("");
  expect(screen.getByText("CATALOG")).toBeInTheDocument();
});

test("while processing a real code+state callback it renders neither the redirect nor an error", () => {
  renderAt("?code=abc&state=xyz");
  expect(screen.queryByText("CATALOG")).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

test("an oidc error is surfaced (and the message shown) instead of hanging blank", () => {
  oidc.error = new Error("invalid_grant");
  renderAt("?code=abc&state=xyz&error=invalid_grant");
  expect(screen.getByRole("alert")).toBeInTheDocument();
  expect(screen.getByText(/sign-in could not be completed/i)).toBeInTheDocument();
  expect(screen.getByText("invalid_grant")).toBeInTheDocument();
});
