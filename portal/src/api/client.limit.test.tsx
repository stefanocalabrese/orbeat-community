/**
 * Task 6 (Community caps, portal side): the 402 body's `limit` object has to
 * survive apiFetch's error path and reach a globally-registered handler from
 * BOTH a failing query and a failing mutation.
 *
 * This file mocks `fetch`, so it proves the client parses and routes the
 * payload, nothing more. It cannot prove a real orbeat-api ever sends one:
 * That proof is Task 7's gate against a generated Community build.
 */
import type { ReactNode } from "react";
import { QueryClientProvider, useMutation } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  apiFetch,
  ApiRequestError,
  createAppQueryClient,
  limitInfo,
  notifyLimitReached,
  setLimitReachedHandler,
} from "./client";

const CAP_BODY = {
  error: { message: "community edition limit reached: servers (10 of 10 used)" },
  limit: { resource: "servers", max: 10, current: 10, contact: "info@orbeat.org" },
};

const json = (body: unknown, status: number) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

beforeEach(() => vi.restoreAllMocks());
afterEach(() => setLimitReachedHandler(null));

test("a 402 body's limit object is carried on the thrown ApiRequestError", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json(CAP_BODY, 402));
  const err = await apiFetch("/v1/catalog", "tok").catch((e: unknown) => e);
  expect(err).toBeInstanceOf(ApiRequestError);
  expect((err as ApiRequestError).status).toBe(402);
  // The prose message still parses, i.e. carrying `limit` did not cost the
  // pre-existing behaviour.
  expect((err as ApiRequestError).message).toBe(CAP_BODY.error.message);
  expect((err as ApiRequestError).limit).toEqual({
    resource: "servers",
    max: 10,
    current: 10,
    contact: "info@orbeat.org",
  });
});

test("the 402 body is read exactly once (a Response stream cannot be consumed twice)", async () => {
  const res = json(CAP_BODY, 402);
  const jsonSpy = vi.spyOn(res, "json");
  vi.spyOn(globalThis, "fetch").mockResolvedValue(res);
  await apiFetch("/v1/catalog", "tok").catch(() => {});
  expect(jsonSpy).toHaveBeenCalledTimes(1);
});

test("a limit object on a non-402 error is ignored (402 is the only cap status)", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json(CAP_BODY, 409));
  const err = (await apiFetch("/v1/admin/servers", "tok", {
    method: "POST",
    body: {},
  }).catch((e: unknown) => e)) as ApiRequestError;
  expect(err.status).toBe(409);
  expect(err.limit).toBeUndefined();
  expect(limitInfo(err)).toBeNull();
});

test("a 402 with a malformed limit is not classified as a cap (no half-rendered dialog)", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json(
      {
        error: { message: "community edition limit reached" },
        // `max` as a string is exactly the shape that would render "10 of 10"
        // while being a payload we did not actually understand.
        limit: { resource: "servers", max: "10", current: 10, contact: "info@orbeat.org" },
      },
      402,
    ),
  );
  const err = (await apiFetch("/v1/catalog", "tok").catch(
    (e: unknown) => e,
  )) as ApiRequestError;
  expect(err.status).toBe(402);
  expect(err.limit).toBeUndefined();
  expect(limitInfo(err)).toBeNull();
  // …and the ordinary error path still has a message to show.
  expect(err.message).toBe("community edition limit reached");
});

test("a 402 whose counts are fractional is not classified as a cap", async () => {
  // JSON can encode this, unlike NaN/Infinity, so it is the reachable half of
  // parseLimit's integer check: `typeof === "number"` would accept it and the
  // dialog would render "2.5 of 10 used".
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json(
      {
        error: { message: "community edition limit reached" },
        limit: { resource: "servers", max: 10, current: 2.5, contact: "info@orbeat.org" },
      },
      402,
    ),
  );
  const err = (await apiFetch("/v1/catalog", "tok").catch(
    (e: unknown) => e,
  )) as ApiRequestError;
  expect(err.status).toBe(402);
  expect(err.limit).toBeUndefined();
  expect(limitInfo(err)).toBeNull();
});

test("a 402 with no body at all is not classified as a cap", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 402 }));
  const err = (await apiFetch("/v1/catalog", "tok").catch(
    (e: unknown) => e,
  )) as ApiRequestError;
  expect(err.status).toBe(402);
  expect(limitInfo(err)).toBeNull();
});

test("limitInfo returns null for anything that is not an ApiRequestError", () => {
  expect(limitInfo(new TypeError("fetch failed"))).toBeNull();
  expect(limitInfo(null)).toBeNull();
  expect(limitInfo({ status: 402, limit: CAP_BODY.limit })).toBeNull();
});

test("notifyLimitReached with no registered handler is a no-op", () => {
  setLimitReachedHandler(null);
  expect(() => notifyLimitReached(CAP_BODY.limit)).not.toThrow();
});

// ── The two wiring paths ─────────────────────────────────────────────────────
// The seat cap fires from the resolver middleware on EVERY authenticated
// request, so the query path is the one a capped user actually hits; the
// server/role caps fire on writes, so the mutation path is the other half.
// Both are asserted, because wiring only one leaves a whole class of cap with
// no dialog.

test("a 402 failing QUERY notifies the limit handler with the parsed payload", async () => {
  const handler = vi.fn();
  setLimitReachedHandler(handler);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json(CAP_BODY, 402));
  const qc = createAppQueryClient();
  await qc
    .fetchQuery({ queryKey: ["catalog"], queryFn: () => apiFetch("/v1/catalog", "t") })
    .catch(() => {});
  expect(handler).toHaveBeenCalledWith({
    resource: "servers",
    max: 10,
    current: 10,
    contact: "info@orbeat.org",
  });
});

test("a 402 failing MUTATION notifies the limit handler with the parsed payload", async () => {
  const handler = vi.fn();
  setLimitReachedHandler(handler);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json(CAP_BODY, 402));
  const qc = createAppQueryClient();
  function wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  }
  const { result } = renderHook(
    () =>
      useMutation({
        mutationFn: () =>
          apiFetch("/v1/admin/servers", "t", { method: "POST", body: {} }),
        retry: false,
      }),
    { wrapper },
  );
  act(() => result.current.mutate());
  await waitFor(() => expect(result.current.isError).toBe(true));
  expect(handler).toHaveBeenCalledWith({
    resource: "servers",
    max: 10,
    current: 10,
    contact: "info@orbeat.org",
  });
});

test("a non-cap failure notifies nothing on either cache", async () => {
  const handler = vi.fn();
  setLimitReachedHandler(handler);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json({ error: { message: "nope" } }, 403),
  );
  const qc = createAppQueryClient();
  await qc
    .fetchQuery({ queryKey: ["catalog"], queryFn: () => apiFetch("/v1/catalog", "t") })
    .catch(() => {});
  expect(handler).not.toHaveBeenCalled();
});
