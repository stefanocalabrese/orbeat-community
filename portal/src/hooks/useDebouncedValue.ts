import { useEffect, useState } from "react";

/**
 * B35: every admin list's search box wired its onChange straight into the
 * query key (useAdminList, api/queries.ts), so typing "github" fired six
 * separate GETs, one per keystroke. 300ms matches this repo's standing
 * search-debounce convention.
 */
export const SEARCH_DEBOUNCE_MS = 300;

/**
 * Returns `value`, but updated only after it has stopped changing for
 * `delayMs`. Lets a page keep the search box's DISPLAY value instant (bound
 * directly to local state, so typing never feels laggy) while the value that
 * actually drives a query key only commits once the user pauses.
 */
export function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}
