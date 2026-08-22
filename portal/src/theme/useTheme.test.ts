import { renderHook, act } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { useTheme } from "./useTheme";

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
});
afterEach(() => vi.restoreAllMocks());

test("read falls back to 'system' when storage getItem throws (blocked storage)", () => {
  // enterprise policy / private mode: getItem throws a SecurityError.
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
    throw new Error("The operation is insecure.");
  });
  // Without the guard this throws during useState(read) and crashes the app.
  const { result } = renderHook(() => useTheme());
  expect(result.current.choice).toBe("system");
});

test("setChoice does not throw when storage setItem is blocked", () => {
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
    throw new Error("The operation is insecure.");
  });
  const { result } = renderHook(() => useTheme());
  act(() => result.current.setChoice("dark"));
  // in-session choice still applied despite the failed persist
  expect(result.current.choice).toBe("dark");
  expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
});

test("reads and persists a valid choice when storage works", () => {
  localStorage.setItem("orbeat-theme", "light");
  const { result } = renderHook(() => useTheme());
  expect(result.current.choice).toBe("light");
  act(() => result.current.setChoice("dark"));
  expect(localStorage.getItem("orbeat-theme")).toBe("dark");
});
