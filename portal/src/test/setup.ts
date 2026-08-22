import "@testing-library/jest-dom/vitest";

// GOTCHA: Node 22+ ships its own experimental global `localStorage` (gated
// behind --localstorage-file; unconfigured, it resolves to `undefined`).
// Vitest's jsdom environment only overrides globals it explicitly lists
// (see vitest's `KEYS`/`getWindowKeys`), and `localStorage` isn't one of
// them — so when Node already defines the property, vitest's jsdom-backed
// Storage never gets installed and `globalThis.localStorage` stays
// `undefined` even though `environment: "jsdom"` is configured. Rebind it to
// the real jsdom Storage instance (exposed via the `jsdom` handle vitest's
// environment sets on `globalThis`) so tests can use the bare `localStorage`
// global like they would in a real browser.
const jsdomInstance = (globalThis as { jsdom?: { window: { localStorage: Storage } } }).jsdom;
if (jsdomInstance) {
  Object.defineProperty(globalThis, "localStorage", {
    get: () => jsdomInstance.window.localStorage,
    configurable: true,
  });
}
