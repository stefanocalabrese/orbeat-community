/**
 * Validate the stored post-login restore target. Only same-origin absolute
 * paths are allowed; anything else — protocol-relative ("//host", and its
 * backslash variant, which browsers normalize to "//"), absolute URLs,
 * relative paths, empty, or absent — falls back to /catalog. The value is
 * self-generated today, but it round-trips through sessionStorage: validating
 * on read keeps a future writer (or a hostile stored value) from turning the
 * signin callback into an open redirect.
 */
export function postLoginTarget(raw: string | null): string {
  if (raw && raw.startsWith("/") && !raw.startsWith("//") && !raw.startsWith("/\\")) {
    return raw;
  }
  return "/catalog";
}
