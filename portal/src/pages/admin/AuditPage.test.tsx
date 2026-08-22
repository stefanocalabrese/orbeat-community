import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import AuditPage from "./AuditPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { "Content-Type": "application/json" } });
const ev = (id: string, action: string) => ({ id, ts: "2026-06-11T10:00:00Z", actor: "boss", action, target: "x", decision: "allow", metadata: {} });
const evWithMeta = (id: string, action: string, metadata: Record<string, unknown>) => ({ ...ev(id, action), metadata });

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    return url.includes("cursor=")
      ? Promise.resolve(json({ events: [ev("2", "older.event")], limit: 50, nextCursor: "" }))
      : Promise.resolve(json({ events: [ev("1", "newest.event")], limit: 50, nextCursor: "abc" }));
  });
});

test("paginates with Load more until cursor exhausted", async () => {
  const user = userEvent.setup();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><AuditPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
  expect(await screen.findByText("newest.event")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /load more/i }));
  expect(await screen.findByText("older.event")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument();
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<AuthCtx.Provider value={boss}><QueryClientProvider client={qc}><MemoryRouter><AuditPage /></MemoryRouter></QueryClientProvider></AuthCtx.Provider>);
}

test("Load more keeps the accumulated rows on screen while the next page is fetching", async () => {
  const user = userEvent.setup();
  let resolveNext: ((r: Response) => void) | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("cursor=")) {
      // hold page 2 in flight so we can observe the in-between state
      return new Promise<Response>((res) => {
        resolveNext = res;
      });
    }
    return Promise.resolve(json({ events: [ev("1", "newest.event")], limit: 50, nextCursor: "abc" }));
  });
  renderPage();
  expect(await screen.findByText("newest.event")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /load more/i }));
  // The accumulated table must NOT unmount while page 2 is in flight…
  expect(screen.getByText("newest.event")).toBeInTheDocument();
  // …and an inline pending indicator shows where Load more was.
  expect(screen.getByText(/loading more/i)).toBeInTheDocument();
  resolveNext!(json({ events: [ev("2", "older.event")], limit: 50, nextCursor: "" }));
  expect(await screen.findByText("older.event")).toBeInTheDocument();
  expect(screen.getByText("newest.event")).toBeInTheDocument();
});

test("a failing next-page fetch keeps the accumulated rows and surfaces the error inline", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input);
    if (url.includes("cursor=")) {
      return Promise.resolve(
        new Response(JSON.stringify({ error: { message: "boom" } }), {
          status: 500,
          headers: { "Content-Type": "application/json" },
        }),
      );
    }
    return Promise.resolve(json({ events: [ev("1", "newest.event")], limit: 50, nextCursor: "abc" }));
  });
  renderPage();
  expect(await screen.findByText("newest.event")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /load more/i }));
  expect(await screen.findByText(/failed to load more audit events/i)).toBeInTheDocument();
  expect(screen.getByText("newest.event")).toBeInTheDocument();
});

test("a failing audit query renders the error panel, not an empty table", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify({ error: { message: "boom" } }), { status: 500, headers: { "Content-Type": "application/json" } })),
  );
  renderPage();
  expect(await screen.findByText(/failed to load audit events/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
});

test("Export JSON fetches the export endpoint with format=json and the date range", async () => {
  const user = userEvent.setup();
  // jsdom lacks these — stub them
  globalThis.URL.createObjectURL = vi.fn(() => "blob:mock");
  globalThis.URL.revokeObjectURL = vi.fn();
  let exportUrl = "";
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/audit/export")) {
      exportUrl = url;
      return Promise.resolve(
        new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }),
      );
    }
    // the audit list fetch
    return Promise.resolve(
      new Response(JSON.stringify({ events: [], limit: 50, nextCursor: "" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
  renderPage(); // use the file's existing render helper (mounts AuditPage with AuthCtx + QueryClient)
  await user.type(await screen.findByLabelText(/from date/i), "2026-07-01");
  await user.type(screen.getByLabelText(/to date/i), "2026-07-12");
  await user.click(screen.getByRole("button", { name: /export json/i }));
  await vi.waitFor(() => expect(exportUrl).toContain("/audit/export"));
  expect(exportUrl).toContain("format=json");
  expect(exportUrl).toContain("from=2026-07-01");
  expect(exportUrl).toContain("to=2026-07-12");
});

test("an inverted date range (from>to) disables export and shows a validation error", async () => {
  const user = userEvent.setup();
  renderPage();
  await user.type(await screen.findByLabelText(/from date/i), "2026-07-12");
  await user.type(screen.getByLabelText(/to date/i), "2026-07-01");
  expect(screen.getByText(/from date must be on or before the to date/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /export json/i })).toBeDisabled();
  expect(screen.getByRole("button", { name: /export csv/i })).toBeDisabled();
});

test("the date range is labeled as an export-only control group", async () => {
  renderPage();
  // a <legend> naming the group so it doesn't read as a table filter
  expect(await screen.findByText(/export range/i)).toBeInTheDocument();
});

// fable-audit §7 #16 item 1: `metadata` arrives on every audit row (arbitrary
// JSON, shape varies per `action`) and was rendered nowhere — the AuditPage
// table stopped at Decision. These two tests pin the expander both ways: no
// control at all when there is genuinely nothing to show, and full
// (including nested) content on demand when there is — the exact
// `role.delete` shape from CHANGELOG v1.24.0 (name/counts/servers[]/
// artifacts[]/truncated).
test("a row with empty metadata renders no expander, just a dash", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(json({ events: [ev("1", "server.create")], limit: 50, nextCursor: "" })),
  );
  renderPage();
  await screen.findByText("server.create");
  expect(screen.queryByRole("button", { name: /details/i })).not.toBeInTheDocument();
  expect(screen.getByText("—")).toBeInTheDocument();
});

test("a row with nested metadata expands to reveal it (keys, array items, and booleans) and collapses back", async () => {
  const user = userEvent.setup();
  const metadata = {
    name: "eng",
    entitlementsRevoked: 3,
    artifactEntitlementsRevoked: 1,
    servers: ["orbeat-gateway", "upstream-x"],
    artifacts: ["release-notes-skill"],
    truncated: false,
  };
  vi.spyOn(globalThis, "fetch").mockImplementation(() =>
    Promise.resolve(json({ events: [evWithMeta("1", "role.delete", metadata)], limit: 50, nextCursor: "" })),
  );
  renderPage();
  await screen.findByText("role.delete");

  const toggle = screen.getByRole("button", { name: /details/i });
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  expect(screen.queryByText("orbeat-gateway")).not.toBeInTheDocument();

  await user.click(toggle);
  expect(toggle).toHaveAttribute("aria-expanded", "true");
  expect(screen.getByText("servers")).toBeInTheDocument();
  expect(screen.getByText("orbeat-gateway")).toBeInTheDocument();
  expect(screen.getByText("upstream-x")).toBeInTheDocument();
  expect(screen.getByText("release-notes-skill")).toBeInTheDocument();
  expect(screen.getByText("3")).toBeInTheDocument();
  expect(screen.getByText("false")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /hide/i }));
  expect(screen.queryByText("orbeat-gateway")).not.toBeInTheDocument();
});
