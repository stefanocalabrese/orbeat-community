/**
 * B15: the audit export used a raw `fetch` with a hand-assembled
 * Authorization header, bypassing apiFetch's hard 30s timeout, its
 * ApiRequestError body parse, the 401 re-login hook, and the 402 cap
 * payload. A hung export never settled and the button stayed enabled the
 * whole time, with no on-screen signal at all.
 *
 * Mocks fetch, like every other portal unit test in this repo — see this
 * file's report for what that can't prove about the real export endpoint.
 */
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach, afterEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import { setUnauthorizedHandler } from "../../api/client";
import AuditPage from "./AuditPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin", "orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function renderPage(fetchImpl: typeof globalThis.fetch) {
  vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><AuditPage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
}

beforeEach(() => vi.restoreAllMocks());
afterEach(() => setUnauthorizedHandler(null));

test("exporting attaches the bearer token via apiFetchRaw's own request path, not a hand-rolled header", async () => {
  const user = userEvent.setup();
  let exportAuth: string | null = null;
  renderPage((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/audit/export")) {
      exportAuth = (init?.headers as Record<string, string> | undefined)?.Authorization ?? null;
      return Promise.resolve(new Response("id,ts\n1,now", { status: 200 }));
    }
    return Promise.resolve(json({ events: [], nextCursor: "" }));
  });
  await user.click(await screen.findByRole("button", { name: /export csv/i }));
  await waitFor(() => expect(exportAuth).toBe("Bearer t"));
});

test("the export button disables while a request is in flight and re-enables when it settles", async () => {
  const user = userEvent.setup();
  let resolveExport: (r: Response) => void = () => {};
  const pending = new Promise<Response>((resolve) => {
    resolveExport = resolve;
  });
  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/audit/export")) return pending;
    return Promise.resolve(json({ events: [], nextCursor: "" }));
  });

  const jsonBtn = await screen.findByRole("button", { name: /export json/i });
  const csvBtn = screen.getByRole("button", { name: /export csv/i });
  await user.click(jsonBtn);

  // Both export buttons — not just the one clicked — must not accept a second
  // overlapping export while one is already in flight.
  await waitFor(() => expect(jsonBtn).toBeDisabled());
  expect(csvBtn).toBeDisabled();

  resolveExport(new Response("[]", { status: 200 }));
  await waitFor(() => expect(jsonBtn).not.toBeDisabled());
  expect(csvBtn).not.toBeDisabled();
});

test("a 401 on export fires the same re-login handler an ordinary query failure would", async () => {
  const user = userEvent.setup();
  const handler = vi.fn();
  setUnauthorizedHandler(handler);
  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/audit/export")) {
      return Promise.resolve(json({ error: { message: "token expired" } }, 401));
    }
    return Promise.resolve(json({ events: [], nextCursor: "" }));
  });
  await user.click(await screen.findByRole("button", { name: /export json/i }));
  await waitFor(() => expect(handler).toHaveBeenCalledTimes(1));
});

test("a hung export that the hard timeout aborts settles the button and shows a signal, rather than leaving it enabled forever", async () => {
  // Simulates what apiFetchRaw's own AbortSignal.timeout(30_000) produces
  // when it actually fires (a fetch rejection with an AbortError) — proving
  // the export's catch/finally handles that outcome, without needing to
  // drive 30 real (or fake-timer) seconds through AbortSignal.timeout
  // itself, which Vitest's fake timers do not intercept.
  const user = userEvent.setup();
  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/audit/export")) {
      return Promise.reject(new DOMException("signal timed out", "TimeoutError"));
    }
    return Promise.resolve(json({ events: [], nextCursor: "" }));
  });

  const btn = await screen.findByRole("button", { name: /export json/i });
  await user.click(btn);

  await waitFor(() => expect(btn).not.toBeDisabled());
  expect(await screen.findByText(/export failed/i)).toBeInTheDocument();
});

test("a non-ok export status surfaces the server's own error message via the ApiRequestError body parse, not a generic HTTP-code string", async () => {
  const user = userEvent.setup();
  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/audit/export")) {
      return Promise.resolve(json({ error: { message: "export window too large" } }, 400));
    }
    return Promise.resolve(json({ events: [], nextCursor: "" }));
  });
  await user.click(await screen.findByRole("button", { name: /export json/i }));
  expect(await screen.findByText(/export window too large/i)).toBeInTheDocument();
});

test("a successful export still downloads the blob (unchanged behaviour)", async () => {
  const user = userEvent.setup();
  const clickSpy = vi.fn();
  const originalCreateElement = document.createElement.bind(document);
  vi.spyOn(document, "createElement").mockImplementation((tag: string) => {
    const el = originalCreateElement(tag);
    if (tag === "a") el.click = clickSpy;
    return el;
  });
  const createObjectURL = vi.fn().mockReturnValue("blob:fake");
  vi.stubGlobal("URL", { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

  renderPage((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/audit/export")) {
      return Promise.resolve(new Response("id,ts\n1,now", { status: 200, headers: { "Content-Type": "text/csv" } }));
    }
    return Promise.resolve(json({ events: [], nextCursor: "" }));
  });
  await user.click(await screen.findByRole("button", { name: /export csv/i }));
  await waitFor(() => expect(clickSpy).toHaveBeenCalledOnce());
  expect(createObjectURL).toHaveBeenCalledOnce();
  vi.unstubAllGlobals();
});
