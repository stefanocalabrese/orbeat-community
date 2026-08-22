import { expect, test, vi, beforeEach, afterEach } from "vitest";
import {
  apiFetch,
  ApiRequestError,
  createAppQueryClient,
  errMsg,
  notifyUnauthorized,
  setUnauthorizedHandler,
} from "./client";

const okJson = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

beforeEach(() => vi.restoreAllMocks());

test("attaches bearer token and parses json", async () => {
  const spy = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(okJson({ servers: [] }));
  const out = await apiFetch<{ servers: unknown[] }>("/v1/catalog", "tok");
  expect(out.servers).toEqual([]);
  const [, init] = spy.mock.calls[0]!;
  expect((init!.headers as Record<string, string>).Authorization).toBe(
    "Bearer tok",
  );
});

test("passes an AbortSignal to fetch even with no caller signal (hard timeout)", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(okJson({}));
  await apiFetch("/x", "tok");
  const [, init] = spy.mock.calls[0]!;
  expect(init!.signal).toBeInstanceOf(AbortSignal);
});

test("combines the caller's signal — aborting it aborts the signal passed to fetch", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(okJson({}));
  const ac = new AbortController();
  await apiFetch("/x", "tok", { signal: ac.signal });
  const [, init] = spy.mock.calls[0]!;
  const combined = init!.signal as AbortSignal;
  expect(combined.aborted).toBe(false);
  ac.abort();
  expect(combined.aborted).toBe(true);
});

test("maps api error body to ApiRequestError with status + message", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    okJson({ error: { message: "already exists" } }, 409),
  );
  await expect(
    apiFetch("/v1/admin/servers", "tok", { method: "POST", body: {} }),
  ).rejects.toMatchObject({ status: 409, message: "already exists" });
});

test("204 returns undefined", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(null, { status: 204 }),
  );
  await expect(
    apiFetch("/x", "tok", { method: "DELETE" }),
  ).resolves.toBeUndefined();
});

// ── Unauthorized handling + query-client policy ─────────────────────────────

afterEach(() => setUnauthorizedHandler(null));

test("two concurrent 401-failing queries fire the unauthorized handler exactly once", async () => {
  const handler = vi.fn();
  setUnauthorizedHandler(handler);
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(okJson({ error: { message: "token expired" } }, 401)),
  );
  const qc = createAppQueryClient();
  await Promise.allSettled([
    qc.fetchQuery({ queryKey: ["a"], queryFn: () => apiFetch("/a", "t") }),
    qc.fetchQuery({ queryKey: ["b"], queryFn: () => apiFetch("/b", "t") }),
  ]);
  expect(handler).toHaveBeenCalledTimes(1);
});

test("the guard re-arms when a handler is re-registered", () => {
  const h1 = vi.fn();
  setUnauthorizedHandler(h1);
  notifyUnauthorized();
  notifyUnauthorized();
  expect(h1).toHaveBeenCalledTimes(1);

  const h2 = vi.fn();
  setUnauthorizedHandler(h2);
  notifyUnauthorized();
  notifyUnauthorized();
  expect(h2).toHaveBeenCalledTimes(1);
});

test("notifyUnauthorized with no registered handler is a no-op", () => {
  setUnauthorizedHandler(null);
  expect(() => notifyUnauthorized()).not.toThrow();
});

test("a 4xx ApiRequestError is not retried (single fetch)", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(okJson({ error: { message: "nope" } }, 404)),
  );
  const qc = createAppQueryClient();
  await qc
    .fetchQuery({ queryKey: ["nf"], queryFn: () => apiFetch("/nf", "t") })
    .catch(() => {});
  expect(spy).toHaveBeenCalledTimes(1);
});

test("a 5xx error is retried a bounded number of times", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(okJson({ error: { message: "down" } }, 500)),
  );
  const qc = createAppQueryClient();
  await qc
    .fetchQuery({
      queryKey: ["srv"],
      queryFn: () => apiFetch("/srv", "t"),
      retryDelay: 0, // keep the default retry POLICY; only collapse the backoff for the test
    })
    .catch(() => {});
  // initial attempt + bounded retries — never the unbounded/3x-default silence
  expect(spy.mock.calls.length).toBeGreaterThan(1);
  expect(spy.mock.calls.length).toBeLessThanOrEqual(3);
});

test("errMsg maps ApiRequestError to its message, other truthy errors to a generic", () => {
  expect(errMsg(new ApiRequestError(409, "already exists"))).toBe("already exists");
  expect(errMsg(new TypeError("fetch failed"))).toBe("request failed");
  expect(errMsg(null)).toBe("");
});

// Type narrowing verification: ApiRequestError must be instanceof-able
test("ApiRequestError is an Error subclass", () => {
  const err = new ApiRequestError(500, "boom");
  expect(err).toBeInstanceOf(Error);
  expect(err).toBeInstanceOf(ApiRequestError);
  expect(err.status).toBe(500);
  expect(err.message).toBe("boom");
});
