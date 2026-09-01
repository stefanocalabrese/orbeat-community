/**
 * B35: admin list search boxes fired one request per keystroke — ListSearchBox's
 * onChange feeds useAdminList's query key directly (queries.ts), so typing
 * "github" issued six separate GETs. useDebouncedValue is what lets each
 * page decouple the box's INSTANT display value from the value that
 * actually drives the query key.
 */
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SEARCH_DEBOUNCE_MS, useDebouncedValue } from "./useDebouncedValue";

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("useDebouncedValue", () => {
  it("returns the initial value immediately", () => {
    const { result } = renderHook(() => useDebouncedValue("git", SEARCH_DEBOUNCE_MS));
    expect(result.current).toBe("git");
  });

  it("does NOT update until the delay elapses", () => {
    const { result, rerender } = renderHook(({ v }) => useDebouncedValue(v, SEARCH_DEBOUNCE_MS), {
      initialProps: { v: "" },
    });
    rerender({ v: "g" });
    act(() => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS - 1);
    });
    expect(result.current).toBe("");
  });

  it("updates once the delay elapses after the value stops changing", () => {
    const { result, rerender } = renderHook(({ v }) => useDebouncedValue(v, SEARCH_DEBOUNCE_MS), {
      initialProps: { v: "" },
    });
    rerender({ v: "g" });
    act(() => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS);
    });
    expect(result.current).toBe("g");
  });

  it("collapses a burst of rapid changes into a single trailing update (one request, not one per keystroke)", () => {
    const { result, rerender } = renderHook(({ v }) => useDebouncedValue(v, SEARCH_DEBOUNCE_MS), {
      initialProps: { v: "" },
    });
    for (const partial of ["g", "gi", "git", "gith", "githu", "github"]) {
      rerender({ v: partial });
      act(() => {
        vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS / 2); // never idle long enough to settle mid-typing
      });
    }
    expect(result.current).toBe(""); // still nothing committed while typing continues
    act(() => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS);
    });
    expect(result.current).toBe("github"); // exactly the final value, once
  });
});
