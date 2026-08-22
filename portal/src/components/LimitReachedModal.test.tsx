/**
 * Task 6 (Community caps, portal side): the 402 dialog and the burst policy
 * that keeps it closable.
 *
 * The payloads below deliberately use a resource name, numbers and an address
 * that appear NOWHERE in the source (`widgets`, 5 of 3, `help@example.test`)
 * rather than the real `servers`/10/`info@orbeat.org`: a dialog that hardcoded
 * the production values would still pass a test written with them.
 *
 * Note what these tests CANNOT show. Every one of them drives the modal or the
 * gate directly, so none can tell whether the gate is mounted in the running
 * app. That is App.test.tsx's wiring gate, which is the only test that fails
 * when <LimitReachedGate /> is deleted from App.tsx.
 */
import { render, screen, cleanup, act, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { LimitInfo } from "../api/types";
import * as client from "../api/client";
import {
  apiFetch,
  createAppQueryClient,
  notifyLimitReached,
  setLimitReachedHandler,
} from "../api/client";
import { LimitReachedModal } from "./LimitReachedModal";
import { LimitReachedGate } from "./LimitReachedGate";

// `current` EXCEEDS `max` on purpose, and it is not a contrived shape: the Go
// caps fire on `current >= max` and report the true count
// (checkServerActiveCap, internal/api/caps.go), so an install already over its
// limit sends current > max. An Enterprise tree regenerated as Community, or
// servers created before the cap existed, both land here. A symmetric fixture
// cannot distinguish the two numbers, so rendering {info.max} in the `current`
// slot would keep every assertion below green.
const widgets: LimitInfo = {
  resource: "widgets",
  max: 3,
  current: 5,
  contact: "help@example.test",
};
const sprockets: LimitInfo = {
  resource: "sprockets",
  max: 7,
  current: 7,
  contact: "other@example.test",
};

beforeEach(() => vi.restoreAllMocks());
afterEach(() => {
  cleanup();
  setLimitReachedHandler(null);
});

// ── The dialog itself ────────────────────────────────────────────────────────

test("renders the resource, the count and the contact from the payload, nothing hardcoded", () => {
  render(<LimitReachedModal info={widgets} onDismiss={() => {}} />);
  const dialog = screen.getByRole("dialog");
  expect(dialog).toHaveTextContent("widgets");
  // The whole sentence, in payload order. Because the fixture is asymmetric
  // (5 used against a max of 3), this fails if the two numbers are swapped or
  // if one is rendered in both slots.
  expect(dialog.textContent).toContain("widgets: 5 of 3 used");
  const mailto = screen.getByRole("link", { name: "help@example.test" });
  expect(mailto).toHaveAttribute("href", "mailto:help@example.test");
});

test("renders a different payload's numbers and address, so none of them are baked in", () => {
  render(<LimitReachedModal info={sprockets} onDismiss={() => {}} />);
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain("sprockets: 7 of 7 used");
  expect(screen.getByRole("link", { name: "other@example.test" })).toHaveAttribute(
    "href",
    "mailto:other@example.test",
  );
  expect(dialog).not.toHaveTextContent("widgets");
});

test("thanks the user and says the free edition's limit is reached (spec §6)", () => {
  render(<LimitReachedModal info={widgets} onDismiss={() => {}} />);
  const dialog = screen.getByRole("dialog");
  expect(dialog).toHaveTextContent(/thank you for using orbeat/i);
  expect(dialog).toHaveTextContent(/free edition limit reached/i);
  // The product name is always lowercase.
  expect(dialog.textContent).toContain("orbeat");
  expect(dialog.textContent).not.toContain("Orbeat");
});

test("is an accessible dialog: aria-modal, an accessible name, and focus moved inside on open", () => {
  render(<LimitReachedModal info={widgets} onDismiss={() => {}} />);
  // getByRole with `name` fails outright if the accessible name is missing,
  // so this asserts aria-labelledby resolves, not merely that a heading exists.
  const dialog = screen.getByRole("dialog", { name: "Free edition limit reached" });
  expect(dialog).toHaveAttribute("aria-modal", "true");
  expect(document.activeElement).toBe(dialog);
});

test("focus returns to whatever was focused before, not to <body>, on close", async () => {
  render(<button type="button">the button that triggered the write</button>);
  const trigger = screen.getByRole("button", { name: /triggered the write/ });
  trigger.focus();
  const { unmount } = render(<LimitReachedModal info={widgets} onDismiss={() => {}} />);
  expect(document.activeElement).toBe(screen.getByRole("dialog"));
  unmount();
  expect(document.activeElement).toBe(trigger);
});

// There is deliberately no test for the restore's `isConnected` guard.
// Measured in this jsdom: focusing a detached element neither throws nor moves
// document.activeElement, so a test for it would pass with the guard deleted.
// See the guard's own comment in LimitReachedModal.tsx.

test("Escape dismisses", async () => {
  const onDismiss = vi.fn();
  const user = userEvent.setup();
  render(<LimitReachedModal info={widgets} onDismiss={onDismiss} />);
  await user.keyboard("{Escape}");
  expect(onDismiss).toHaveBeenCalledTimes(1);
});

test("Escape still dismisses when focus is outside the dialog", async () => {
  const onDismiss = vi.fn();
  const user = userEvent.setup();
  render(
    <>
      <button type="button">outside</button>
      <LimitReachedModal info={widgets} onDismiss={onDismiss} />
    </>,
  );
  screen.getByRole("button", { name: "outside" }).focus();
  expect(document.activeElement).not.toBe(screen.getByRole("dialog"));
  await user.keyboard("{Escape}");
  expect(onDismiss).toHaveBeenCalledTimes(1);
});

// ── Focus containment ────────────────────────────────────────────────────────
// aria-modal="true" tells assistive tech the rest of the page is inert. These
// are the tests that make that claim true rather than a lie. They are also the
// reason this dialog is a div and not the platform <dialog>: showModal() is
// undefined in jsdom 30.0.1 (measured), so a native implementation could not
// be covered here at all.

test("Tab from the last focusable element wraps to the first, never to the page behind", async () => {
  const user = userEvent.setup();
  render(
    <>
      <button type="button">behind the scrim</button>
      <LimitReachedModal info={widgets} onDismiss={() => {}} />
    </>,
  );
  const dialog = screen.getByRole("dialog");
  const close = screen.getByRole("button", { name: "Close" });
  const mailto = screen.getByRole("link", { name: "help@example.test" });
  close.focus();
  await user.tab();
  expect(dialog).toContainElement(document.activeElement as HTMLElement);
  expect(document.activeElement).toBe(mailto);
});

test("Shift+Tab from the dialog container wraps to the last focusable, never to the page behind", async () => {
  const user = userEvent.setup();
  render(
    <>
      <button type="button">behind the scrim</button>
      <LimitReachedModal info={widgets} onDismiss={() => {}} />
    </>,
  );
  const dialog = screen.getByRole("dialog");
  expect(document.activeElement).toBe(dialog);
  await user.tab({ shift: true });
  expect(dialog).toContainElement(document.activeElement as HTMLElement);
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" }));
});

test("Tab pulls focus back in when it is already outside the dialog", async () => {
  const user = userEvent.setup();
  // TWO buttons behind the scrim, adjacent in DOM order, and that is what
  // makes this test able to fail. With one, default Tab from it happens to
  // land on the dialog's first link anyway, so the assertion passed with the
  // recover branch deleted. With two, a handler that does nothing lets Tab
  // move to the SECOND outside button, which is the bug.
  render(
    <>
      <button type="button">behind one</button>
      <button type="button">behind two</button>
      <LimitReachedModal info={widgets} onDismiss={() => {}} />
    </>,
  );
  const dialog = screen.getByRole("dialog");
  screen.getByRole("button", { name: "behind one" }).focus();
  await user.tab();
  expect(document.activeElement).not.toBe(screen.getByRole("button", { name: "behind two" }));
  expect(dialog).toContainElement(document.activeElement as HTMLElement);
  expect(document.activeElement).toBe(screen.getByRole("link", { name: "help@example.test" }));
});

test("repeated tabbing never lands on the page behind the scrim", async () => {
  const user = userEvent.setup();
  render(
    <>
      <button type="button">behind the scrim</button>
      <LimitReachedModal info={widgets} onDismiss={() => {}} />
    </>,
  );
  const dialog = screen.getByRole("dialog");
  const outside = screen.getByRole("button", { name: "behind the scrim" });
  for (let i = 0; i < 8; i++) {
    await user.tab();
    expect(document.activeElement).not.toBe(outside);
    expect(dialog).toContainElement(document.activeElement as HTMLElement);
  }
});

test("the Close button dismisses", async () => {
  const onDismiss = vi.fn();
  const user = userEvent.setup();
  render(<LimitReachedModal info={widgets} onDismiss={onDismiss} />);
  await user.click(screen.getByRole("button", { name: "Close" }));
  expect(onDismiss).toHaveBeenCalledTimes(1);
});

test("the Escape listener is removed on unmount", async () => {
  const onDismiss = vi.fn();
  const user = userEvent.setup();
  const { unmount } = render(
    <LimitReachedModal info={widgets} onDismiss={onDismiss} />,
  );
  unmount();
  await user.keyboard("{Escape}");
  expect(onDismiss).not.toHaveBeenCalled();
});

// ── The gate: registration and the burst policy ──────────────────────────────

test("the gate renders nothing until a cap is reported", () => {
  render(<LimitReachedGate />);
  expect(screen.queryByRole("dialog")).toBeNull();
  act(() => notifyLimitReached(widgets));
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

// THIS is the first-402-wins test, and the only shape that can be one. A burst
// of 20 notifications carrying the SAME payload is indistinguishable in the
// DOM whichever notification wins, so the obvious "a burst opens exactly one
// dialog" test survived all 17 red-proof mutations and was deleted rather than
// kept: a test that cannot fail for the reason it names is worse than none.
// Two DIFFERENT resources is the only arrangement where first-wins and
// latest-wins disagree, so this test carries the rule by itself.
test("a second cap arriving while the dialog is open does not swap the payload under the reader", () => {
  render(<LimitReachedGate />);
  act(() => notifyLimitReached(widgets));
  act(() => notifyLimitReached(sprockets));
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain("widgets: 5 of 3 used");
  expect(dialog).not.toHaveTextContent("sprockets");
});

test("after dismissal, further 402s for the same resource do NOT reopen it (the 2s poll must not trap the user)", async () => {
  const user = userEvent.setup();
  render(<LimitReachedGate />);
  act(() => notifyLimitReached(widgets));
  await user.click(screen.getByRole("button", { name: "Close" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  // The marketplace-status poll keeps 402ing every two seconds.
  act(() => {
    for (let i = 0; i < 5; i++) notifyLimitReached(widgets);
  });
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("dismissal is per-resource: a DIFFERENT cap still opens after one was dismissed", async () => {
  const user = userEvent.setup();
  render(<LimitReachedGate />);
  act(() => notifyLimitReached(widgets));
  await user.click(screen.getByRole("button", { name: "Close" }));
  act(() => notifyLimitReached(sprockets));
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain("sprockets: 7 of 7 used");
});

// Asserted through a spy on the setter, NOT by "unmount, notify, expect no
// dialog": React 19 makes a setState on an unmounted component a silent no-op,
// so that shape passes identically with the cleanup deleted. The only
// observable difference between having the cleanup and not is the null call.
test("the gate registers its handler on mount and unregisters it on unmount", () => {
  const spy = vi.spyOn(client, "setLimitReachedHandler");
  const { unmount } = render(<LimitReachedGate />);
  expect(spy).toHaveBeenCalledTimes(1);
  expect(spy.mock.calls[0]?.[0]).toBeTypeOf("function");
  unmount();
  expect(spy).toHaveBeenLastCalledWith(null);
});

test("a real 402 through the app query client opens the dialog end to end", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        error: { message: "community edition limit reached: seats (10 of 10 used)" },
        limit: { resource: "seats", max: 10, current: 10, contact: "info@orbeat.org" },
      }),
      { status: 402, headers: { "Content-Type": "application/json" } },
    ),
  );
  render(<LimitReachedGate />);
  const qc = createAppQueryClient();
  await act(async () => {
    await qc
      .fetchQuery({ queryKey: ["catalog"], queryFn: () => apiFetch("/v1/catalog", "t") })
      .catch(() => {});
  });
  await waitFor(() =>
    expect(screen.getByRole("dialog").textContent).toContain("seats: 10 of 10 used"),
  );
  expect(screen.getByRole("link", { name: "info@orbeat.org" })).toHaveAttribute(
    "href",
    "mailto:info@orbeat.org",
  );
});
