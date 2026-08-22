import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, test } from "vitest";
import ThemeToggle from "./ThemeToggle";

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
});

test("defaults to system (no data-theme attribute) and marks System active", async () => {
  render(<ThemeToggle />);
  expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  expect(screen.getByRole("button", { name: /system/i })).toHaveAttribute("aria-pressed", "true");
});

test("selecting Dark stamps data-theme=dark and persists", async () => {
  const user = userEvent.setup();
  render(<ThemeToggle />);
  await user.click(screen.getByRole("button", { name: /^dark$/i }));
  expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  expect(localStorage.getItem("orbeat-theme")).toBe("dark");
});

test("reads a persisted choice on mount", async () => {
  localStorage.setItem("orbeat-theme", "light");
  render(<ThemeToggle />);
  expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  expect(screen.getByRole("button", { name: /^light$/i })).toHaveAttribute("aria-pressed", "true");
});
