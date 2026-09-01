/**
 * Task 8 (docs/plans/orbeat-virtual-keys-2026-08-25.md, spec §11): the
 * virtual-keys admin console page.
 *
 * Mocks `fetch`, like every other portal unit test in this repo. This is
 * evidence the PORTAL wires the field/route/body shape correctly, never
 * that the real API accepts what the form sends or that the routes exist;
 * that is Task 10's smoke and e2e work.
 */
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import VirtualKeysPage from "./VirtualKeysPage";

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
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const meOn = {
  subject: "boss",
  email: "b@x",
  roles: ["orbeat-admin"],
  features: { pinning: true, virtualKeys: true },
};
const meOff = {
  subject: "boss",
  email: "b@x",
  roles: ["orbeat-admin"],
  features: { pinning: true, virtualKeys: false },
};

const roleRows = [
  { id: "role-1", name: "ops" },
  { id: "role-2", name: "ci-robots" },
];

function renderPage(fetchImpl: typeof globalThis.fetch) {
  vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const utils = render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <VirtualKeysPage />
        </MemoryRouter>
      </QueryClientProvider>
    </AuthCtx.Provider>,
  );
  return { ...utils, qc };
}

beforeEach(() => vi.restoreAllMocks());

// router discriminates by URL/method and answers every endpoint the content
// component could reach; used everywhere below, INCLUDING the gate-3
// "false" test, on purpose: a single-endpoint stub that only answers GET
// /v1/me would let a gate-removal mutant crash-render instead of visibly
// rendering the page, which would make the "renders nothing" assertion
// pass for the wrong reason (a thrown error mid-render, not an intact,
// correctly-hidden tree). `me` defaults to meOn so every OTHER test, which
// wants the content to mount, doesn't have to pass it.
function router(opts: {
  me?: unknown;
  keys?: unknown[];
  onPost?: (init?: RequestInit) => Promise<Response>;
  onDelete?: (url: string, init?: RequestInit) => Promise<Response>;
}) {
  return (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? "GET";
    if (url.endsWith("/v1/me")) return Promise.resolve(json(opts.me ?? meOn));
    if (method === "POST" && url.endsWith("/v1/admin/virtual-keys")) {
      return opts.onPost
        ? opts.onPost(init)
        : Promise.reject(new Error("unexpected POST"));
    }
    if (method === "DELETE" && url.includes("/v1/admin/virtual-keys/")) {
      return opts.onDelete
        ? opts.onDelete(url, init)
        : Promise.reject(new Error("unexpected DELETE"));
    }
    if (url.includes("/v1/admin/roles")) {
      return Promise.resolve(json({ roles: roleRows, limit: 100, nextCursor: "" }));
    }
    if (url.includes("/v1/admin/virtual-keys")) {
      return Promise.resolve(json({ virtualKeys: opts.keys ?? [], limit: 100, nextCursor: "" }));
    }
    return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
  };
}

// ---- Gate 3: the whole page renders nothing, both while loading and when off ----

test("renders nothing while GET /v1/me is still loading", () => {
  // A Promise that never resolves: useMe() stays isPending forever, which is
  // exactly the state useVirtualKeysEnabled must resolve to "hidden" for,
  // the same as an explicit false; no fetch below ever needs to settle.
  const { container } = renderPage(() => new Promise(() => {}));
  expect(container).toBeEmptyDOMElement();
});

test("renders nothing once GET /v1/me resolves reporting features.virtualKeys: false", async () => {
  const { container, qc } = renderPage(router({ me: meOff, keys: [] }));
  await waitFor(() => expect(qc.getQueryState(["me"])?.status).toBe("success"));
  expect(container).toBeEmptyDOMElement();
});

// B35: a genuine GET /v1/me FAILURE (network blip, backend error — not
// Community, and not still loading) used to collapse into the exact same
// silent `null` as the two cases above, indistinguishable from an admin's
// point of view. The nav link is now hidden in this case too (AdminLayout),
// but a stale bookmark or a typed URL still reaches this page directly, so
// it must explain itself rather than render a blank pane.
test("shows an explanatory message, not a blank pane, when GET /v1/me itself fails", async () => {
  renderPage(() =>
    Promise.resolve(json({ error: { message: "gateway timeout" } }, 502)),
  );
  expect(
    await screen.findByText(/could not determine whether virtual keys are available/i),
  ).toBeInTheDocument();
  expect(screen.getByText(/gateway timeout/)).toBeInTheDocument();
});

// ---- Happy path baseline: virtualKeys: true renders the console page ----

test("renders the console page when features.virtualKeys is true", async () => {
  renderPage(router({ keys: [] }));
  expect(await screen.findByRole("heading", { name: "Virtual keys" })).toBeInTheDocument();
});

// ---- Gate 1 + Gate 4: create posts the exact body, and narrowing round-trips ----

test("create posts name, description, roleId, the parsed JWKS object and the narrowed tool list as the EXACT request body", async () => {
  const user = userEvent.setup();
  let postBody: unknown;
  renderPage(
    router({
      keys: [],
      onPost: (init) => {
        postBody = JSON.parse(String(init?.body));
        return Promise.resolve(
          json(
            {
              id: "vk-1",
              clientId: "vk-client-1",
              roleId: "role-2",
              name: "ci-bot",
              description: "nightly release job",
              allowedTools: ["echo", "create_issue"],
              revoked: false,
              createdAt: "2026-08-25T00:00:00Z",
              rowVersion: 1,
            },
            201,
          ),
        );
      },
    }),
  );

  await user.click(await screen.findByRole("button", { name: "New virtual key" }));
  await user.type(screen.getByLabelText("Name"), "ci-bot");
  await user.type(screen.getByLabelText("Description"), "nightly release job");
  // Explicit selection (not the default first option) proves the SELECTED
  // role is what is sent, not whatever happens to sit first in the list.
  await user.selectOptions(screen.getByLabelText("Role"), "role-2");
  fireEvent.change(screen.getByLabelText("Public JWKS"), {
    target: { value: JSON.stringify({ kty: "oct", k: "robot-public-key-material" }) },
  });
  await user.type(screen.getByLabelText("Narrowed tools"), "echo, create_issue");
  await user.click(screen.getByRole("button", { name: "Create" }));

  await vi.waitFor(() => expect(postBody).toBeDefined());
  expect(postBody).toEqual({
    name: "ci-bot",
    description: "nightly release job",
    roleId: "role-2",
    jwks: { kty: "oct", k: "robot-public-key-material" },
    allowedTools: ["echo", "create_issue"],
  });
});

test("the narrowing round-trips: typing three tools produces three tools, in the order typed, not a constant", async () => {
  // Review finding this red-proof is written against directly (see the task
  // brief): a test that types ONE tool and asserts ONE tool cannot tell
  // "sends what was typed" apart from "sends a hardcoded constant", the
  // exact coincidence that let a sibling portal test pass while its
  // component hardcoded minRevision: 1. Three tools in a deliberately
  // NON-alphabetical order also catches a mutant that sorts the array.
  const user = userEvent.setup();
  let postBody: { allowedTools?: string[] } | undefined;
  renderPage(
    router({
      keys: [],
      onPost: (init) => {
        postBody = JSON.parse(String(init?.body));
        return Promise.resolve(
          json(
            {
              id: "vk-2", clientId: "vk-client-2", roleId: "role-1", name: "n",
              description: "", allowedTools: [], revoked: false,
              createdAt: "2026-08-25T00:00:00Z", rowVersion: 1,
            },
            201,
          ),
        );
      },
    }),
  );

  await user.click(await screen.findByRole("button", { name: "New virtual key" }));
  await user.type(screen.getByLabelText("Name"), "n");
  await user.selectOptions(screen.getByLabelText("Role"), "role-1");
  fireEvent.change(screen.getByLabelText("Public JWKS"), {
    target: { value: JSON.stringify({ kty: "oct", k: "x" }) },
  });
  await user.type(screen.getByLabelText("Narrowed tools"), "zulu_tool, echo, alpha_tool");
  await user.click(screen.getByRole("button", { name: "Create" }));

  await vi.waitFor(() => expect(postBody).toBeDefined());
  expect(postBody!.allowedTools).toEqual(["zulu_tool", "echo", "alpha_tool"]);
});

test("leaving narrowed tools empty sends allowedTools: null (everything the role allows)", async () => {
  const user = userEvent.setup();
  let postBody: { allowedTools?: string[] | null } | undefined;
  renderPage(
    router({
      keys: [],
      onPost: (init) => {
        postBody = JSON.parse(String(init?.body));
        return Promise.resolve(
          json(
            {
              id: "vk-3", clientId: "vk-client-3", roleId: "role-1", name: "n",
              description: "", revoked: false, createdAt: "2026-08-25T00:00:00Z", rowVersion: 1,
            },
            201,
          ),
        );
      },
    }),
  );

  await user.click(await screen.findByRole("button", { name: "New virtual key" }));
  await user.type(screen.getByLabelText("Name"), "n");
  await user.selectOptions(screen.getByLabelText("Role"), "role-1");
  fireEvent.change(screen.getByLabelText("Public JWKS"), {
    target: { value: JSON.stringify({ kty: "oct", k: "x" }) },
  });
  await user.click(screen.getByRole("button", { name: "Create" }));

  await vi.waitFor(() => expect(postBody).toBeDefined());
  expect(postBody!.allowedTools).toBeNull();
});

test("invalid JSON in the JWKS field is refused inline and never reaches fetch", async () => {
  const user = userEvent.setup();
  let posted = false;
  renderPage(
    router({
      keys: [],
      onPost: () => {
        posted = true;
        return Promise.resolve(json({ error: { message: "should not be called" } }, 400));
      },
    }),
  );

  await user.click(await screen.findByRole("button", { name: "New virtual key" }));
  await user.type(screen.getByLabelText("Name"), "n");
  await user.selectOptions(screen.getByLabelText("Role"), "role-1");
  fireEvent.change(screen.getByLabelText("Public JWKS"), { target: { value: "not json" } });
  await user.click(screen.getByRole("button", { name: "Create" }));

  expect(await screen.findByText(/jwks must be valid json/i)).toBeInTheDocument();
  expect(posted).toBe(false);
});

// ---- Gate 2: revoke sends the quoted If-Match from the row's rowVersion ----

const existingKey = {
  id: "vk-9",
  clientId: "vk-client-9",
  roleId: "role-1",
  name: "nightly-ci",
  description: "release automation",
  allowedTools: ["echo"],
  revoked: false,
  createdAt: "2026-08-20T10:00:00Z",
  rowVersion: 7,
};

test("revoke: confirming sends DELETE for the row's id with a quoted If-Match from its rowVersion", async () => {
  const user = userEvent.setup();
  const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
  let deleteUrl: string | undefined;
  let deleteIfMatch: string | null = null;
  let deleteCalls = 0;
  renderPage(
    router({
      keys: [existingKey],
      onDelete: (url, init) => {
        deleteCalls += 1;
        deleteUrl = url;
        deleteIfMatch = new Headers(init?.headers).get("If-Match");
        return Promise.resolve(json({ id: "vk-9", clientId: "vk-client-9", revoked: true }));
      },
    }),
  );

  await screen.findByText("nightly-ci");
  await user.click(screen.getByRole("button", { name: "Revoke nightly-ci" }));

  expect(confirmSpy).toHaveBeenCalledWith(
    'Revoke virtual key "nightly-ci"? Its robot is rejected on its very next call.',
  );
  await vi.waitFor(() => expect(deleteCalls).toBe(1));
  expect(deleteUrl).toMatch(/\/v1\/admin\/virtual-keys\/vk-9$/);
  // Quoted, matching the strong ETag the server emits; an unquoted
  // If-Match is a 400 (internal/api's ifMatch parser).
  expect(deleteIfMatch).toBe('"7"');
});

test("revoke: dismissing the confirm issues no request", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(false);
  let deleteCalls = 0;
  renderPage(
    router({
      keys: [existingKey],
      onDelete: () => {
        deleteCalls += 1;
        return Promise.reject(new Error("DELETE must not be issued when the confirm is dismissed"));
      },
    }),
  );

  await screen.findByText("nightly-ci");
  await user.click(screen.getByRole("button", { name: "Revoke nightly-ci" }));

  expect(deleteCalls).toBe(0);
});

test("a revoked key has no Revoke button", async () => {
  renderPage(router({ keys: [{ ...existingKey, revoked: true }] }));

  await screen.findByText("nightly-ci");
  expect(screen.queryByRole("button", { name: "Revoke nightly-ci" })).not.toBeInTheDocument();
  expect(screen.getByText("revoked")).toBeInTheDocument();
});
