import { test, expect } from "vitest";
import { postLoginTarget } from "./postLoginTarget";

// The stored post-login target is self-generated today, but it round-trips
// through sessionStorage — one refactor away from attacker influence. Only
// same-origin absolute paths may be restored; anything else falls back to
// /catalog so a hostile value can never become an open redirect.

test("restores a same-origin absolute path", () => {
  expect(postLoginTarget("/admin/audit")).toBe("/admin/audit");
});

test("null (nothing stored) falls back to /catalog", () => {
  expect(postLoginTarget(null)).toBe("/catalog");
});

test("a protocol-relative URL (//evil.example) is rejected", () => {
  expect(postLoginTarget("//evil.example/phish")).toBe("/catalog");
});

test("an absolute URL (https://evil.example) is rejected", () => {
  expect(postLoginTarget("https://evil.example/phish")).toBe("/catalog");
});

test("a relative path without a leading slash is rejected", () => {
  expect(postLoginTarget("admin/audit")).toBe("/catalog");
});

test("an empty string is rejected", () => {
  expect(postLoginTarget("")).toBe("/catalog");
});

test("backslash variant of protocol-relative (/\\evil.example) is rejected", () => {
  // Browsers normalize backslashes to forward slashes in URLs, so "/\\host"
  // behaves like "//host" — treat it as hostile too.
  expect(postLoginTarget("/\\evil.example")).toBe("/catalog");
});
