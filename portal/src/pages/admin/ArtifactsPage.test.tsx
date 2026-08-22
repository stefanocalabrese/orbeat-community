import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { vi, test, expect, beforeEach } from "vitest";
import { AuthCtx } from "../../auth/useAuth";
import ArtifactsPage from "./ArtifactsPage";

const boss = { isLoading: false, authenticated: true, roles: ["orbeat-admin", "orbeat-user"], token: "t", login: () => {}, logout: () => {}, subject: "boss", email: "b@x" };
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const artifact = { id: "1", type: "skill" as const, name: "fmt", description: "formats code", content: "---\nname: fmt\ndescription: formats code\n---\nbody", memoryScope: null, memorySeed: null, version: "0.1.0", approvalState: "approved" as const, approved: true, visibility: "org" as const };
const publishStatus = { lastAttemptAt: null, lastSuccessAt: null, lastCommit: "abc123", lastError: "" };

function renderPage(fetchImpl?: typeof globalThis.fetch) {
  if (fetchImpl) {
    vi.spyOn(globalThis, "fetch").mockImplementation(fetchImpl);
  }
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const utils = render(
    <AuthCtx.Provider value={boss}>
      <QueryClientProvider client={qc}><MemoryRouter><ArtifactsPage /></MemoryRouter></QueryClientProvider>
    </AuthCtx.Provider>,
  );
  // Exposed so tests can reach into the cache directly (e.g. to seed a
  // warm read deterministically) instead of racing TanStack Query's own
  // gcTime/staleTime timers.
  return { ...utils, qc };
}

beforeEach(() => vi.restoreAllMocks());

test("lists artifact by name and type badge", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [artifact], limit: 100, nextCursor: "" }));
  });
  renderPage();
  expect(await screen.findByText("fmt")).toBeInTheDocument();
  expect(screen.getByText("skill")).toBeInTheDocument();
});

test("lists a rule artifact labeled 'rule' with its own chip styling (not skill's)", async () => {
  const rule = { ...artifact, id: "r1", type: "rule" as const, name: "org-std", visibility: "role" as const };
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [artifact, rule], limit: 100, nextCursor: "" }));
  });
  renderPage();
  expect(await screen.findByText("org-std")).toBeInTheDocument();
  const ruleChip = screen.getByText("rule");
  const skillChip = screen.getByText("skill");
  expect(ruleChip.className).not.toMatch(/undefined/);
  // a rule must not be dressed as a skill
  expect(ruleChip.className).not.toBe(skillChip.className);
});

test("a failing artifacts query renders the error panel, not an empty table", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ error: { message: "boom" } }, 500));
  });
  renderPage();
  expect(await screen.findByText(/failed to load artifacts/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
});

test("publish-status banner renders lastCommit", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  expect(await screen.findByText(/abc123/)).toBeInTheDocument();
});

test("create: selecting type=subagent reveals memoryScope select", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  // memoryScope should not be visible yet (type defaults to skill)
  expect(screen.queryByLabelText(/memory scope/i)).not.toBeInTheDocument();
  // switch type to subagent
  await user.selectOptions(screen.getByLabelText(/^type$/i), "subagent");
  // memoryScope select should now be visible
  expect(screen.getByLabelText(/memory scope/i)).toBeInTheDocument();
});

test("create: type selector offers rule and selecting it hides memory fields", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  // the type selector offers a `rule` option
  expect(screen.getByRole("option", { name: "rule" })).toBeInTheDocument();
  // selecting rule must NOT reveal the subagent-only memory-scope/seed fields
  await user.selectOptions(screen.getByLabelText(/^type$/i), "rule");
  expect(screen.queryByLabelText(/memory scope/i)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/seed memory/i)).not.toBeInTheDocument();
});

test("create: content hint switches with the type — rule warns against frontmatter", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  // skill (default) keeps the frontmatter hint
  expect(screen.getByText(/Markdown with YAML frontmatter/i)).toBeInTheDocument();
  // rule must NOT be told to write frontmatter — it is delivered verbatim into AGENTS.md
  await user.selectOptions(screen.getByLabelText(/^type$/i), "rule");
  expect(screen.queryByText(/Markdown with YAML frontmatter/i)).not.toBeInTheDocument();
  expect(screen.getByText(/no YAML frontmatter/i)).toBeInTheDocument();
  expect(screen.getByText(/AGENTS\.md/)).toBeInTheDocument();
  // switching back restores the skill/subagent hint
  await user.selectOptions(screen.getByLabelText(/^type$/i), "subagent");
  expect(screen.getByText(/Markdown with YAML frontmatter/i)).toBeInTheDocument();
});

test("create form has a visibility selector defaulting to org", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  const sel = screen.getByLabelText("Visibility") as HTMLSelectElement;
  expect(sel.value).toBe("org");
  expect(screen.getByRole("option", { name: "role" })).toBeInTheDocument();
});

test("create: submitting a skill POSTs to /v1/admin/artifacts", async () => {
  const user = userEvent.setup();
  let postBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "POST" && url.includes("/artifacts")) {
      postBody = JSON.parse(String(init.body));
      return Promise.resolve(json({ artifacts: [artifact], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  await user.type(screen.getByLabelText(/^name$/i), "fmt");
  await user.type(screen.getByLabelText(/description/i), "formats code");
  await user.type(screen.getByLabelText(/content/i), "---\nname: fmt\ndescription: formats code\n---\nbody");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  await vi.waitFor(() => expect(postBody).toBeDefined());
  expect((postBody as Record<string, string>).name).toBe("fmt");
});

test("create: 400 error surfaces inline", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "POST") {
      return Promise.resolve(json({ error: { message: "memoryScope is only valid for subagents" } }, 400));
    }
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  await user.type(screen.getByLabelText(/^name$/i), "fmt");
  await user.type(screen.getByLabelText(/content/i), "---\nname: fmt\ndescription: d\n---\nbody");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  expect(await screen.findByText(/memoryScope is only valid for subagents/)).toBeInTheDocument();
});

test("reopening the create form after a failed submit clears the stale error", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "POST") return Promise.resolve(json({ error: { message: "already exists" } }, 409));
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  await user.type(screen.getByLabelText(/^name$/i), "fmt");
  await user.type(screen.getByLabelText(/content/i), "---\nname: fmt\ndescription: d\n---\nbody");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  expect(await screen.findByText(/already exists/)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /new artifact/i }));
  expect(screen.queryByText(/already exists/)).not.toBeInTheDocument();
});

test("row Edit/Delete buttons carry name-scoped accessible labels", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [artifact], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await screen.findByText("fmt");
  expect(screen.getByRole("button", { name: "Edit fmt" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Delete fmt" })).toBeInTheDocument();
});

test("create: switching type back to skill clears memoryScope before submit", async () => {
  const user = userEvent.setup();
  let postBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "POST" && url.includes("/artifacts")) {
      postBody = JSON.parse(String(init.body));
      return Promise.resolve(json({ artifacts: [artifact], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  // select subagent and set memoryScope to "user"
  await user.selectOptions(screen.getByLabelText(/^type$/i), "subagent");
  await user.selectOptions(screen.getByLabelText(/memory scope/i), "user");
  await user.type(screen.getByLabelText(/seed memory/i), "org seed");
  // switch back to skill — memoryScope select disappears
  await user.selectOptions(screen.getByLabelText(/^type$/i), "skill");
  expect(screen.queryByLabelText(/memory scope/i)).not.toBeInTheDocument();
  // fill required fields and submit
  await user.type(screen.getByLabelText(/^name$/i), "fmt");
  await user.type(screen.getByLabelText(/description/i), "formats code");
  await user.type(screen.getByLabelText(/content/i), "---\nname: fmt\ndescription: formats code\n---\nbody");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  await vi.waitFor(() => expect(postBody).toBeDefined());
  // memoryScope must be cleared — NOT "user" — so the backend does not reject with 400
  expect((postBody as Record<string, string>).memoryScope).toBe("");
  // memorySeed must be cleared too — belt-and-suspenders on the type-flip clearing path
  expect((postBody as Record<string, string>).memorySeed).toBe("");
});

test("create: seed textarea appears only for user/project-scope subagents", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  await user.selectOptions(screen.getByLabelText(/^type$/i), "subagent");
  // no scope yet → no seed field
  expect(screen.queryByLabelText(/seed memory/i)).not.toBeInTheDocument();
  await user.selectOptions(screen.getByLabelText(/memory scope/i), "user");
  expect(screen.getByLabelText(/seed memory/i)).toBeInTheDocument();
  await user.selectOptions(screen.getByLabelText(/memory scope/i), "local");
  expect(screen.queryByLabelText(/seed memory/i)).not.toBeInTheDocument();
});

test("create: changing memoryScope away from user/project clears memorySeed in the POST", async () => {
  const user = userEvent.setup();
  let postBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "POST" && url.includes("/artifacts")) {
      postBody = JSON.parse(String(init.body));
      return Promise.resolve(json({ artifacts: [artifact], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  await user.selectOptions(screen.getByLabelText(/^type$/i), "subagent");
  await user.selectOptions(screen.getByLabelText(/memory scope/i), "user");
  await user.type(screen.getByLabelText(/seed memory/i), "org seed");
  // scope moves to local — the seed must be auto-cleared (backend would 400)
  await user.selectOptions(screen.getByLabelText(/memory scope/i), "local");
  await user.type(screen.getByLabelText(/^name$/i), "rev");
  await user.type(screen.getByLabelText(/description/i), "d");
  await user.type(screen.getByLabelText(/content/i), "---\nname: rev\ndescription: d\n---\nbody");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  await vi.waitFor(() => expect(postBody).toBeDefined());
  expect((postBody as Record<string, string>).memorySeed).toBe("");
});

test("create: memorySeed is posted for a user-scope subagent", async () => {
  const user = userEvent.setup();
  let postBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "POST" && url.includes("/artifacts")) {
      postBody = JSON.parse(String(init.body));
      return Promise.resolve(json({ artifacts: [artifact], limit: 100, nextCursor: "" }));
    }
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /new artifact/i }));
  await user.selectOptions(screen.getByLabelText(/^type$/i), "subagent");
  await user.selectOptions(screen.getByLabelText(/memory scope/i), "user");
  await user.type(screen.getByLabelText(/seed memory/i), "org seed");
  await user.type(screen.getByLabelText(/^name$/i), "rev");
  await user.type(screen.getByLabelText(/description/i), "d");
  await user.type(screen.getByLabelText(/content/i), "---\nname: rev\ndescription: d\n---\nbody");
  await user.click(screen.getByRole("button", { name: /^create$/i }));
  await vi.waitFor(() => expect(postBody).toBeDefined());
  expect((postBody as Record<string, string>).memorySeed).toBe("org seed");
});

test("a failing delete surfaces its error near the heading (not silently discarded)", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "DELETE") {
      return Promise.resolve(json({ error: { message: "artifact is entitled to a role" } }, 409));
    }
    return Promise.resolve(json({ artifacts: [artifact], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await screen.findByText("fmt");
  await user.click(screen.getByRole("button", { name: /^delete /i }));
  expect(await screen.findByText(/artifact is entitled to a role/)).toBeInTheDocument();
});

test("a rejected Republish surfaces its error in the publish banner", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/publish") && init?.method === "POST") {
      return Promise.resolve(json({ error: { message: "git target unreachable" } }, 502));
    }
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByRole("button", { name: /republish/i }));
  expect(await screen.findByText(/git target unreachable/)).toBeInTheDocument();
});

test("shows the approval-state badge and a Submit action for a draft", async () => {
  const draft = {
    id: "a1",
    type: "skill" as const,
    name: "sk",
    description: "d",
    content: "c",
    memoryScope: null,
    memorySeed: null,
    version: "",
    approvalState: "draft" as const,
    approved: false,
    visibility: "org" as const,
  };
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [draft], limit: 100, nextCursor: "" }));
  });
  renderPage();
  expect(await screen.findByText("draft")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /submit/i })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /withdraw/i })).not.toBeInTheDocument();
});

test("an approved artifact shows Withdraw and not Submit", async () => {
  const approved = {
    id: "a2",
    type: "skill" as const,
    name: "live",
    description: "d",
    content: "c",
    memoryScope: null,
    memorySeed: null,
    version: "1.0.0",
    approvalState: "approved" as const,
    approved: true,
    visibility: "org" as const,
  };
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    return Promise.resolve(json({ artifacts: [approved], limit: 100, nextCursor: "" }));
  });
  renderPage();
  expect(await screen.findByText("live")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /withdraw/i })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^submit$/i })).not.toBeInTheDocument();
});

test("edit: a seeded subagent's seed shows in the form and survives save", async () => {
  const user = userEvent.setup();
  const seeded = { ...artifact, id: "9", type: "subagent" as const, name: "rev", memoryScope: "user", memorySeed: "org seed", visibility: "role" as const };
  let putBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    // The edit form now fetches the full artifact by id (Task 11 / C8 #2)
    // rather than prefilling from the slim list row — a distinct GET.
    if (init?.method === "GET" && url.endsWith("/artifacts/9")) {
      return Promise.resolve(json(seeded));
    }
    if (init?.method === "PUT" && url.includes("/artifacts/9")) {
      putBody = JSON.parse(String(init.body));
      return Promise.resolve(json(seeded));
    }
    return Promise.resolve(json({ artifacts: [seeded], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await user.click(await screen.findByText("Edit"));
  const seedField = (await screen.findByLabelText(/seed memory/i)) as HTMLTextAreaElement;
  expect(seedField.value).toBe("org seed");
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  await vi.waitFor(() => expect(putBody).toBeDefined());
  expect((putBody as Record<string, string>).memorySeed).toBe("org seed");
  expect((putBody as Record<string, string>).memoryScope).toBe("user");
});

test("history: opens a revision list and rolls back a prior version", async () => {
  const user = userEvent.setup();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  const approved = { ...artifact, id: "h1", name: "hist", approvalState: "approved" as const, approved: true };
  const revisions = [
    { revision: 2, source: "approval", content: "V2", approvedBy: "bob", approvedAt: "2026-07-12T10:00:00Z", isCurrent: true },
    { revision: 1, source: "approval", content: "V1", approvedBy: "bob", approvedAt: "2026-07-12T09:00:00Z", isCurrent: false },
  ];
  let rollbackBody: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/rollback") && init?.method === "POST") {
      rollbackBody = JSON.parse(String(init.body));
      return Promise.resolve(json(approved));
    }
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifacts: [approved], limit: 100, nextCursor: "" }));
  });
  renderPage();

  const row = within((await screen.findByText("hist")).closest("tr") as HTMLElement);
  await user.click(await row.findByRole("button", { name: /history/i }));

  // both revisions listed; #2 marked current (no rollback button), #1 rollable
  expect(await screen.findByText(/#2/)).toBeInTheDocument();
  expect(screen.getByText(/current/i)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /roll back to #1/i }));
  await vi.waitFor(() => expect(rollbackBody).toBeDefined());
  expect((rollbackBody as { revision: number }).revision).toBe(1);
});

test("edit: switching from artifact A to artifact B and back to A remounts with A's values (unkeyed-form regression)", async () => {
  const user = userEvent.setup();
  const artifactA = { ...artifact, id: "1", name: "fmt", content: "content-A" };
  const artifactB = { ...artifact, id: "2", name: "lint", content: "content-B" };
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    // The edit form now fetches the full artifact by id (Task 11 / C8 #2).
    if (init?.method === "GET" && url.endsWith("/artifacts/1")) return Promise.resolve(json(artifactA));
    if (init?.method === "GET" && url.endsWith("/artifacts/2")) return Promise.resolve(json(artifactB));
    return Promise.resolve(json({ artifacts: [artifactA, artifactB], limit: 100, nextCursor: "" }));
  });
  const { qc } = renderPage();
  await screen.findByText("fmt");

  const [editA, editB] = screen.getAllByRole("button", { name: /^edit /i });
  if (!editA || !editB) throw new Error("expected two Edit buttons");
  await user.click(editA);
  expect((await screen.findByLabelText(/^name$/i)) as HTMLInputElement).toHaveProperty("value", "fmt");
  expect((screen.getByLabelText(/content/i) as HTMLTextAreaElement).value).toBe("content-A");

  await user.click(editB);
  const nameField = (await screen.findByLabelText(/^name$/i)) as HTMLInputElement;
  const contentField = screen.getByLabelText(/content/i) as HTMLTextAreaElement;
  expect(nameField.value).toBe("lint");
  expect(contentField.value).toBe("content-B");

  // Task 11 review, Important #3: reopen A a third time. With gcTime: 0
  // (Important #2) the query cache no longer reliably keeps a warm entry
  // around across a real unmount, so a test relying on that timing would
  // be racing a gc(0) timer. Seed the cache SYNCHRONOUSLY right before the
  // click instead — this deterministically reproduces the one shape that
  // actually matters for the `key={mode.id}` invariant: data already
  // available at render time, no intervening "Loading artifact…" frame.
  // Without the key, React would reconcile the SAME <ArtifactForm> element
  // in place (same component type, same position, no key change) and its
  // useState(initial) — set once at B's mount — would silently keep
  // showing B's values despite the new `initial` prop.
  qc.setQueryData(["admin", "artifacts", "byId", "1"], artifactA);
  await user.click(editA);
  const nameField2 = (await screen.findByLabelText(/^name$/i)) as HTMLInputElement;
  expect(nameField2.value).toBe("fmt");
  expect((screen.getByLabelText(/content/i) as HTMLTextAreaElement).value).toBe("content-A");
});

test("edit: reopening artifact A after viewing B always re-fetches — never shows a stale cached value", async () => {
  const user = userEvent.setup();
  const artifactA = { ...artifact, id: "1", name: "fmt", content: "content-A" };
  const artifactAChanged = { ...artifactA, content: "content-A-CHANGED-BY-ANOTHER-ADMIN" };
  const artifactB = { ...artifact, id: "2", name: "lint", content: "content-B" };
  let aFetchCount = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (init?.method === "GET" && url.endsWith("/artifacts/1")) {
      aFetchCount++;
      return Promise.resolve(json(aFetchCount === 1 ? artifactA : artifactAChanged));
    }
    if (init?.method === "GET" && url.endsWith("/artifacts/2")) return Promise.resolve(json(artifactB));
    return Promise.resolve(json({ artifacts: [artifactA, artifactB], limit: 100, nextCursor: "" }));
  });
  renderPage();
  await screen.findByText("fmt");

  const [editA, editB] = screen.getAllByRole("button", { name: /^edit /i });
  if (!editA || !editB) throw new Error("expected two Edit buttons");

  await user.click(editA);
  expect(await screen.findByLabelText(/content/i)).toHaveValue("content-A");

  await user.click(editB);
  await screen.findByDisplayValue("content-B");

  // Another admin changed artifact A server-side while we had B open.
  // `vi.waitFor` is deliberately NOT used for the final check here: it has
  // no knowledge of React and would resolve as soon as `aFetchCount` ticks
  // to 2 (which happens the instant the fetch is DISPATCHED, not once its
  // result has been rendered) — a synchronous DOM read right after could
  // then run before React ever processes the response. `findByDisplayValue`
  // polls the real DOM (act-aware), so it only succeeds once the fresh
  // value has actually landed.
  await user.click(editA);
  expect(await screen.findByDisplayValue("content-A-CHANGED-BY-ANOTHER-ADMIN")).toBeInTheDocument();
  expect(aFetchCount).toBe(2);
});

test("history: a failing revisions query shows an error, not the no-versions empty state", async () => {
  const user = userEvent.setup();
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ error: { message: "boom" } }, 500));
    return Promise.resolve(json({ artifacts: [artifact], limit: 100, nextCursor: "" }));
  });
  renderPage();
  const row = within((await screen.findByText("fmt")).closest("tr") as HTMLElement);
  await user.click(await row.findByRole("button", { name: /history/i }));
  expect(await screen.findByText(/failed to load revisions/i)).toBeInTheDocument();
  expect(screen.queryByText(/no approved versions/i)).not.toBeInTheDocument();
});

test("history: shows empty-state when an artifact has no approved versions", async () => {
  const user = userEvent.setup();
  const draft = { ...artifact, id: "d1", name: "drafty", approvalState: "draft" as const, approved: false };
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions: [], limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifacts: [draft], limit: 100, nextCursor: "" }));
  });
  renderPage();
  const row = within((await screen.findByText("drafty")).closest("tr") as HTMLElement);
  await user.click(await row.findByRole("button", { name: /history/i }));
  expect(await screen.findByText(/no approved versions/i)).toBeInTheDocument();
});

// fable-audit §7 #16 item 2: ArtifactRevision.content was already in the
// GET .../revisions payload and rendered nowhere (ArtifactsPage.tsx used to
// list only revision metadata — number, source, approver, timestamp). These
// two tests pin the line-level diff that now surfaces it: a real added/
// removed line pair between two revisions, and the single-pane fallback for
// the one revision that has no predecessor to diff against.
test("history: View changes on revision #2 highlights the exact line that differs from #1", async () => {
  const user = userEvent.setup();
  const approved = { ...artifact, id: "h2", name: "histdiff", approvalState: "approved" as const, approved: true };
  const revisions = [
    { revision: 2, source: "approval", content: "alpha\nbeta\ngamma", approvedBy: "bob", approvedAt: "2026-07-12T10:00:00Z", isCurrent: true },
    { revision: 1, source: "approval", content: "alpha\nBETA\ngamma", approvedBy: "bob", approvedAt: "2026-07-12T09:00:00Z", isCurrent: false },
  ];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifacts: [approved], limit: 100, nextCursor: "" }));
  });
  renderPage();
  const row = within((await screen.findByText("histdiff")).closest("tr") as HTMLElement);
  await user.click(await row.findByRole("button", { name: /history/i }));
  await screen.findByText(/#2/);

  // Scope to revision #2's own list item — #1 renders its own "View
  // changes" control too (single-pane, asserted below in the sibling test),
  // and the two must not collide.
  const rev2Item = within((await screen.findByText(/#2/)).closest("li") as HTMLElement);
  const toggle = rev2Item.getByRole("button", { name: /view changes/i });
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  await user.click(toggle);
  expect(toggle).toHaveAttribute("aria-expanded", "true");

  // Unchanged lines appear on both sides; the changed line is present as
  // BOTH its old (removed) and new (added) form — never collapsed away,
  // never shown as a single opaque block (the pre-fix behavior).
  expect(screen.getAllByText("alpha").length).toBeGreaterThanOrEqual(2);
  expect(screen.getAllByText("gamma").length).toBeGreaterThanOrEqual(2);
  expect(screen.getByText("BETA")).toBeInTheDocument();
  expect(screen.getByText("beta")).toBeInTheDocument();
});

test("history: revision #1 has no predecessor — View changes shows one undiffed content pane, not a diff", async () => {
  const user = userEvent.setup();
  const approved = { ...artifact, id: "h3", name: "histfirst", approvalState: "approved" as const, approved: true };
  const revisions = [
    { revision: 1, source: "approval", content: "only-ever-version", approvedBy: "bob", approvedAt: "2026-07-12T09:00:00Z", isCurrent: true },
  ];
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor: "" }));
    return Promise.resolve(json({ artifacts: [approved], limit: 100, nextCursor: "" }));
  });
  renderPage();
  const row = within((await screen.findByText("histfirst")).closest("tr") as HTMLElement);
  await user.click(await row.findByRole("button", { name: /history/i }));
  await user.click(await screen.findByRole("button", { name: /view changes/i }));

  expect(await screen.findByText("only-ever-version")).toBeInTheDocument();
  // Exactly one DiffPane label ("Revision #1") rendered inside the panel —
  // two would mean it silently diffed against an empty string instead of
  // falling back to a single pane.
  const panel = document.getElementById("revision-diff-1") as HTMLElement;
  expect(within(panel).getAllByText(/^Revision #1$/)).toHaveLength(1);
});

// Revision pruning (docs/specs/2026-08-19-orbeat-revision-pruning-design.md
// §6). ORBEAT_ARTIFACT_REVISION_KEEP caps artifact_revision at the newest N
// and removes a PREFIX, so the oldest survivor is #9 with #8 permanently
// absent, and a rollback's restored_from_num — a plain int column, not a
// foreign key — can point at a row that is gone. Both cases previously
// rendered as "not loaded yet": a View-changes button that never appeared,
// and a live pointer to a revision the admin cannot find. The four tests
// below pin the pruned case AND the unpaged case it must not swallow.
const prunedHistory = (revisions: unknown[], nextCursor: string) => {
  const approved = { ...artifact, id: "hp", name: "histcap", approvalState: "approved" as const, approved: true };
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes("/marketplace/status")) return Promise.resolve(json(publishStatus));
    if (url.includes("/revisions")) return Promise.resolve(json({ revisions, limit: 100, nextCursor }));
    return Promise.resolve(json({ artifacts: [approved], limit: 100, nextCursor: "" }));
  });
  return approved;
};

const rev = (revision: number, extra: Record<string, unknown> = {}) => ({
  revision,
  source: "approval",
  content: `content-${revision}`,
  approvedBy: "bob",
  approvedAt: "2026-07-12T09:00:00Z",
  isCurrent: false,
  ...extra,
});

async function openHistory(name: string) {
  const user = userEvent.setup();
  renderPage();
  const row = within((await screen.findByText(name)).closest("tr") as HTMLElement);
  await user.click(await row.findByRole("button", { name: /history/i }));
  return user;
}

test("history: the oldest surviving revision after pruning offers View changes as a single undiffed pane", async () => {
  // KEEP=2 left {#9,#10}; #8 was pruned and nextCursor "" says no page can
  // ever hold it. #9 must render the same single-pane view as revision 1 —
  // under the pre-pruning gate its button never appeared at all.
  prunedHistory([rev(10, { isCurrent: true }), rev(9)], "");
  const user = await openHistory("histcap");

  const rev9 = within((await screen.findByText("#9")).closest("li") as HTMLElement);
  await user.click(rev9.getByRole("button", { name: /view changes/i }));

  const panel = document.getElementById("revision-diff-9") as HTMLElement;
  expect(within(panel).getByText("content-9")).toBeInTheDocument();
  // Exactly one DiffPane label: two would mean it diffed #9 against an empty
  // string and showed the whole revision as "added".
  expect(within(panel).getAllByText(/^Revision #9$/)).toHaveLength(1);
});

test("history: a predecessor that is merely unpaged still offers no View changes", async () => {
  // Same shape, but nextCursor is non-empty — #8 may still arrive via "Load
  // more revisions", so the pruned fallback must NOT fire here.
  prunedHistory([rev(10, { isCurrent: true }), rev(9)], "cur");
  await openHistory("histcap");

  const rev9 = within((await screen.findByText("#9")).closest("li") as HTMLElement);
  expect(rev9.queryByRole("button", { name: /view changes/i })).not.toBeInTheDocument();
  // #10's predecessor IS loaded, so it keeps its diff either way.
  const rev10 = within(screen.getByText("#10").closest("li") as HTMLElement);
  expect(rev10.getByRole("button", { name: /view changes/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /load more revisions/i })).toBeInTheDocument();
});

test("history: a rollback whose restored-from revision was pruned renders as pruned, not as a live pointer", async () => {
  prunedHistory([rev(11, { source: "rollback", restoredFrom: 3, isCurrent: true }), rev(10)], "");
  await openHistory("histcap");

  expect(await screen.findByText("rollback of #3 (pruned)")).toBeInTheDocument();
  // The bare pointer must be gone, not merely accompanied: #3 is not in the
  // list and no page can bring it in.
  expect(screen.queryByText("rollback of #3")).not.toBeInTheDocument();
});

test("history: a rollback target that is merely unpaged is not labelled pruned", async () => {
  prunedHistory([rev(11, { source: "rollback", restoredFrom: 3, isCurrent: true }), rev(10)], "cur");
  await openHistory("histcap");

  expect(await screen.findByText("rollback of #3")).toBeInTheDocument();
  expect(screen.queryByText(/\(pruned\)/)).not.toBeInTheDocument();
});
