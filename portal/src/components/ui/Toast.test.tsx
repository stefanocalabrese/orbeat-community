import { render, screen, waitFor, act, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { vi, test, expect, afterEach } from "vitest";
import { ToastProvider } from "./Toast";
import { useToast, TOAST_TTL_MS } from "./toastContext";

afterEach(() => vi.useRealTimers());

function Pusher({ message, kind }: { message: string; kind?: "success" | "error" }) {
  const { push } = useToast();
  return (
    <button type="button" onClick={() => push(message, kind)}>
      do it
    </button>
  );
}

test("a pushed toast is announced and can be dismissed by hand", async () => {
  const user = userEvent.setup();
  render(
    <ToastProvider>
      <Pusher message="Role deleted." />
    </ToastProvider>,
  );
  await user.click(screen.getByRole("button", { name: /do it/i }));

  const live = await screen.findByRole("status");
  expect(live).toHaveAttribute("aria-live", "polite");
  expect(live).toHaveTextContent("Role deleted.");

  await user.click(screen.getByRole("button", { name: /dismiss: role deleted/i }));
  await waitFor(() => expect(screen.queryByRole("status")).not.toBeInTheDocument());
});

test("a toast dismisses itself after its TTL", () => {
  vi.useFakeTimers();
  render(
    <ToastProvider>
      <Pusher message="Approved and distributed." />
    </ToastProvider>,
  );
  // fireEvent rather than userEvent: userEvent schedules its own work on the
  // clock, and pairing it with fake timers turns this into a test about the two
  // libraries agreeing rather than about the toast expiring.
  act(() => {
    fireEvent.click(screen.getByRole("button", { name: /do it/i }));
  });
  expect(screen.getByRole("status")).toHaveTextContent("Approved and distributed.");

  // Driven by the fake clock rather than by waiting: asserting "it is gone
  // after four real seconds" would measure the machine.
  act(() => {
    vi.advanceTimersByTime(TOAST_TTL_MS + 10);
  });
  expect(screen.queryByRole("status")).not.toBeInTheDocument();
});

test("useToast outside a provider is a no-op, not a throw", () => {
  // Dozens of existing page tests render without a ToastProvider, and every
  // mutation hook calls useToast. Throwing here would fail suites that have
  // nothing to do with toasts, and would make a mutation refuse to run because
  // nobody could be told it worked.
  expect(() => render(<Pusher message="x" />)).not.toThrow();
});
