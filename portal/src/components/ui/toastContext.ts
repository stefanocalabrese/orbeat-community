import { createContext, useContext } from "react";

/**
 * Toasts exist for the half of a mutation nobody was told about: FAILURE is
 * already reported inline on every admin page, but SUCCESS was silent. A row
 * vanished, a form closed, and the user inferred the rest.
 *
 * Deliberately narrow. A toast carries a sentence and disappears; anything a
 * user must act on (a 412 conflict, a 402 cap) stays where it is, inline or in
 * a dialog, because a message that auto-dismisses is the wrong place for a
 * decision.
 */
export type ToastKind = "success" | "error";

export interface Toast {
  id: number;
  kind: ToastKind;
  message: string;
}

interface ToastCtxValue {
  toasts: Toast[];
  push: (message: string, kind?: ToastKind) => void;
  dismiss: (id: number) => void;
}

export const ToastCtx = createContext<ToastCtxValue | null>(null);

/** How long a toast stays up before dismissing itself. */
export const TOAST_TTL_MS = 4000;


/**
 * useToast returns a no-op push outside a provider rather than throwing.
 *
 * That is not laziness about setup: useInvalidating (api/queries.ts) calls this
 * from EVERY mutation hook, and dozens of existing component tests render a
 * single page with no ToastProvider around it. Throwing would turn "this page
 * has no toast host" into a failing test suite that says nothing about the page
 * under test, and a mutation refusing to run because nobody could be told it
 * worked would be the notification tail wagging the dog.
 */
export function useToast(): ToastCtxValue {
  return useContext(ToastCtx) ?? noopToasts;
}

const noopToasts: ToastCtxValue = { toasts: [], push: () => {}, dismiss: () => {} };

