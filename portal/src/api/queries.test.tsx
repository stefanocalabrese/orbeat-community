/**
 * Task 10 (optimistic concurrency, portal side): the three mutations that hit
 * enforcing endpoints (`PUT /v1/admin/servers/{id}`, `PUT
 * /v1/admin/artifacts/{id}`, `POST /v1/admin/artifacts/{id}/approve`) must
 * send `If-Match: "<rowVersion>"` — quoted, matching the strong entity-tag
 * the server emits (an unquoted value is a 400) — and a 412 response must
 * surface as an `ApiRequestError` with `status === 412` so a future Task 11
 * conflict-UI branch has something concrete to switch on.
 *
 * This file mocks `fetch`: it proves the CLIENT constructs the right request
 * and interprets the right response, nothing more. It cannot prove the real
 * seam — there is no CORS preflight, no real server enforcing the
 * precondition, no real `ETag`/`rowVersion` round-trip. That proof is Task
 * 12's Playwright spec (`portal/e2e/concurrency.spec.ts`) against the real
 * compose stack; do not read a green run here as evidence the browser can
 * actually do this against `orbeat-api`.
 */
import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, expect, test, vi } from "vitest";
import { AuthCtx } from "../auth/useAuth";
import { ApiRequestError } from "./client";
import { useApproveArtifact, useUpdateArtifact, useUpdateServer } from "./queries";
import type { ArtifactInput, ServerInput } from "./types";

const boss = {
  isLoading: false,
  authenticated: true,
  roles: ["orbeat-admin", "orbeat-user"],
  token: "t",
  login: () => {},
  logout: () => {},
  subject: "boss",
  email: "b@x",
};

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return (
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    </AuthCtx.Provider>
  );
}

function ifMatchHeader(init: RequestInit | undefined): string | undefined {
  return (init?.headers as Record<string, string> | undefined)?.["If-Match"];
}

const serverInput: ServerInput = {
  name: "github",
  description: "",
  transport: "http",
  endpointOrCommand: "https://x/mcp",
  version: "",
  protocolVersion: "",
  secretRef: "",
  tlsCaRef: "",
  status: "active",
};

const artifactInput: ArtifactInput = {
  type: "skill",
  name: "fmt",
  description: "",
  content: "body",
  memoryScope: "",
  memorySeed: "",
  version: "",
  visibility: "org",
};

beforeEach(() => vi.restoreAllMocks());

test("useUpdateServer sends a quoted If-Match built from the given rowVersion", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ id: "1", rowVersion: 2 }));
  const { result } = renderHook(() => useUpdateServer(), { wrapper });

  await act(async () => {
    await result.current.mutateAsync({ id: "1", input: serverInput, rowVersion: 1 });
  });

  expect(ifMatchHeader(spy.mock.calls[0]?.[1])).toBe('"1"');
});

test("useUpdateServer's 412 surfaces as an ApiRequestError with status 412", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json({ error: { message: "row changed" } }, 412),
  );
  const { result } = renderHook(() => useUpdateServer(), { wrapper });

  act(() => {
    result.current.mutate({ id: "1", input: serverInput, rowVersion: 1 });
  });

  await waitFor(() => expect(result.current.isError).toBe(true));
  expect(result.current.error).toBeInstanceOf(ApiRequestError);
  expect((result.current.error as ApiRequestError).status).toBe(412);
});

test("useUpdateArtifact sends a quoted If-Match built from the given rowVersion", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ id: "9", rowVersion: 4 }));
  const { result } = renderHook(() => useUpdateArtifact(), { wrapper });

  await act(async () => {
    await result.current.mutateAsync({ id: "9", input: artifactInput, rowVersion: 3 });
  });

  expect(ifMatchHeader(spy.mock.calls[0]?.[1])).toBe('"3"');
});

test("useUpdateArtifact's 412 surfaces as an ApiRequestError with status 412", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json({ error: { message: "row changed" } }, 412),
  );
  const { result } = renderHook(() => useUpdateArtifact(), { wrapper });

  act(() => {
    result.current.mutate({ id: "9", input: artifactInput, rowVersion: 3 });
  });

  await waitFor(() => expect(result.current.isError).toBe(true));
  expect(result.current.error).toBeInstanceOf(ApiRequestError);
  expect((result.current.error as ApiRequestError).status).toBe(412);
});

test("useApproveArtifact sends a quoted If-Match built from the given rowVersion", async () => {
  const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ id: "9", rowVersion: 6 }));
  const { result } = renderHook(() => useApproveArtifact(), { wrapper });

  await act(async () => {
    await result.current.mutateAsync({ id: "9", rowVersion: 5 });
  });

  expect(ifMatchHeader(spy.mock.calls[0]?.[1])).toBe('"5"');
});

test("useApproveArtifact's 412 surfaces as an ApiRequestError with status 412 (the content approved isn't what was reviewed)", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json({ error: { message: "row changed" } }, 412),
  );
  const { result } = renderHook(() => useApproveArtifact(), { wrapper });

  act(() => {
    result.current.mutate({ id: "9", rowVersion: 5 });
  });

  await waitFor(() => expect(result.current.isError).toBe(true));
  expect(result.current.error).toBeInstanceOf(ApiRequestError);
  expect((result.current.error as ApiRequestError).status).toBe(412);
});
