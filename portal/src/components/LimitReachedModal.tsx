import { useEffect, useRef } from "react";
import type { LimitInfo } from "../api/types";
import { Button } from "./ui/Button";

/**
 * The Community-edition cap dialog
 * (docs/specs/2026-08-19-orbeat-community-caps-design.md §6): it thanks the
 * user, says the free edition's limit is reached, and shows the contact.
 *
 * Every number, the resource name and the address come from `info`, which
 * apiFetch parsed out of the 402 body. Nothing about the limit is written
 * into this file, so the API stays the single source of the limit and the
 * count: raising a cap or repointing ORBEAT_CONTACT_EMAIL needs no portal
 * change, and the dialog can never claim a number the server did not send.
 *
 * This is the first dialog in portal/src (there was no modal primitive
 * before it), so it sets the precedent. Dependency-free on purpose:
 *
 *  - role="dialog" + aria-modal="true" + aria-labelledby on the heading, so
 *    assistive tech announces it by name and treats the rest of the page as
 *    inert. `aria-modal` is a CLAIM, and the Tab trap below is what makes it
 *    true: asserting it while Tab still reached the page behind would tell
 *    assistive tech the background is inert when it is not, which is worse
 *    than not claiming modality at all.
 *  - Focus moves onto the dialog itself (tabIndex={-1}) on open, so keyboard
 *    and screen-reader users start inside it instead of at the top of the
 *    page behind it, and is RESTORED to whatever was focused before on close.
 *    Without the restore, dismissing drops focus to <body> and a keyboard user
 *    has to tab from the top of the page again.
 *  - Escape closes and Tab wraps, both from one `document` listener. Document
 *    level rather than the dialog's own onKeyDown because the trap has to be
 *    able to recover focus that is ALREADY outside the dialog: a
 *    dialog-scoped handler never runs in exactly that case.
 *
 * WHY NOT THE PLATFORM <dialog> + showModal(), which would give containment,
 * top-layer stacking and inertness for free: it is unavailable in this test
 * environment. Measured against the installed jsdom 30.0.1, not assumed from
 * its changelog: `HTMLDialogElement` exists as a class and the `open`
 * attribute reflects, but `showModal`, `show` and `close` are ALL `undefined`.
 * A `showModal()` call would throw in every test, and shimming the three
 * methods in the shared test setup would leave the containment assertions
 * measuring the shim rather than a browser. The div plus a real trap is the
 * standard pre-<dialog> modal pattern, it is dependency-free, and it is
 * provable here: see the Tab-containment tests, which fail if the trap is
 * removed. Revisit when jsdom implements the element.
 */
export function LimitReachedModal({
  info,
  onDismiss,
}: {
  info: LimitInfo;
  onDismiss: () => void;
}) {
  const dialogRef = useRef<HTMLDivElement>(null);

  // Capture-then-restore, both in one effect so the restore is the cleanup of
  // the very capture it pairs with. `document.activeElement` is read before
  // the focus() call below, i.e. still the element the user was on when the
  // cap fired.
  useEffect(() => {
    const previous = document.activeElement;
    dialogRef.current?.focus();
    // `instanceof HTMLElement` is the load-bearing half: activeElement is
    // typed Element | null, and .focus() on null throws. `isConnected` is
    // defensive only, and knowingly unfalsifiable here: focusing a detached
    // element was measured in this jsdom to neither throw nor move
    // activeElement, so no test can fail for its absence. It is kept because
    // refocusing a node that has left the document is meaningless, not
    // because anything proves it necessary.
    return () => {
      if (previous instanceof HTMLElement && previous.isConnected) previous.focus();
    };
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        onDismiss();
        return;
      }
      if (e.key !== "Tab") return;
      const root = dialogRef.current;
      if (!root) return;
      const focusable = [
        ...root.querySelectorAll<HTMLElement>(
          'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
        ),
      ];
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      const active = document.activeElement;
      // No focusable content at all: keep the caret on the dialog itself
      // rather than letting Tab escape to the page behind.
      if (!first || !last) {
        e.preventDefault();
        root.focus();
        return;
      }
      // Focus is somewhere outside the dialog (moved programmatically, or the
      // browser restored it after an alt-tab). Pull it back in.
      if (!(active instanceof Node) || !root.contains(active)) {
        e.preventDefault();
        first.focus();
        return;
      }
      // Wrap at the two ends. The dialog container itself counts as "before
      // the first" for Shift+Tab, since focus starts there on open.
      if (e.shiftKey && (active === first || active === root)) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onDismiss]);

  // The scrim's bg-black/50 is the one colour here that is NOT a theme token,
  // and deliberately so: every token in this file (bg-surface, text-text,
  // border-border) flips between the light and dark palettes, but a scrim has
  // to darken whatever is behind it in BOTH. A token that inverted would
  // become a white wash over the dark theme and stop reading as "the page
  // behind is inert".
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4">
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="limit-reached-title"
        tabIndex={-1}
        // Card (ui/Card.tsx) carries this exact class list, but it types its
        // props as HTMLAttributes<HTMLDivElement>, which has no `ref` for the
        // focus effect above to attach. Re-spelled here rather than widening
        // Card for one caller.
        className="w-full max-w-md rounded-xl border border-border bg-surface p-6 shadow-sm"
      >
        <h2 id="limit-reached-title" className="text-lg font-semibold text-text">
          Free edition limit reached
        </h2>
        <p className="mt-3 text-sm text-muted">Thank you for using orbeat.</p>
        {/* Phrased as "<resource>: N of M used" rather than "using N of M
            <resource>" so the sentence never has to agree with the count. The
            role cap is exactly 1 (editionLimits.Roles, internal/api), so the
            natural phrasing would render "1 of 1 roles" on a real cap. */}
        <p className="mt-2 text-sm text-muted">
          <span className="text-text">{info.resource}</span>:{" "}
          <span className="font-mono text-text">{info.current}</span> of{" "}
          <span className="font-mono text-text">{info.max}</span> used, the most
          the free edition allows.
        </p>
        <p className="mt-2 text-sm text-muted">
          To lift the limit, write to{" "}
          <a
            className="font-medium text-accent underline"
            href={`mailto:${info.contact}`}
          >
            {info.contact}
          </a>
          .
        </p>
        <div className="mt-5 flex justify-end">
          <Button type="button" variant="ghost" onClick={onDismiss}>
            Close
          </Button>
        </div>
      </div>
    </div>
  );
}
