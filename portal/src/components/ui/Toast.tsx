import { useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ToastCtx, TOAST_TTL_MS, type Toast, type ToastKind } from "./toastContext";

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const nextId = useRef(1);
  // Timers are tracked so unmounting cannot leave a setState pointed at a
  // component that is gone, and so dismissing by hand cancels the pending
  // auto-dismiss rather than leaving a timer to fire on a missing id.
  const timers = useRef(new Map<number, ReturnType<typeof setTimeout>>());

  const dismiss = useCallback((id: number) => {
    const t = timers.current.get(id);
    if (t !== undefined) {
      clearTimeout(t);
      timers.current.delete(id);
    }
    setToasts((prev) => prev.filter((x) => x.id !== id));
  }, []);

  const push = useCallback(
    (message: string, kind: ToastKind = "success") => {
      const id = nextId.current++;
      setToasts((prev) => [...prev, { id, kind, message }]);
      timers.current.set(
        id,
        setTimeout(() => dismiss(id), TOAST_TTL_MS),
      );
    },
    [dismiss],
  );

  useEffect(() => {
    const pending = timers.current;
    return () => {
      pending.forEach((t) => clearTimeout(t));
      pending.clear();
    };
  }, []);

  const value = useMemo(() => ({ toasts, push, dismiss }), [toasts, push, dismiss]);
  return (
    <ToastCtx.Provider value={value}>
      {children}
      <ToastViewport />
    </ToastCtx.Provider>
  );
}

function ToastViewport() {
  const ctx = useContext(ToastCtx);
  if (!ctx || ctx.toasts.length === 0) return null;
  return (
    // aria-live="polite" so a screen reader announces the outcome without
    // interrupting; "status" rather than "alert" because a success is not an
    // emergency and an error here always has an inline twin that is.
    <div
      role="status"
      aria-live="polite"
      className="pointer-events-none fixed bottom-4 right-4 z-50 flex w-80 flex-col gap-2"
    >
      {ctx.toasts.map((t) => (
        <div
          key={t.id}
          className={`pointer-events-auto flex items-start gap-3 rounded-lg border px-3 py-2 text-sm shadow-lg ${
            t.kind === "error"
              ? "border-danger bg-danger-weak text-danger"
              : "border-border-strong bg-surface text-text"
          }`}
        >
          <span className="flex-1">{t.message}</span>
          <button
            type="button"
            aria-label={`Dismiss: ${t.message}`}
            onClick={() => ctx.dismiss(t.id)}
            className="text-faint hover:text-text"
          >
            ×
          </button>
        </div>
      ))}
    </div>
  );
}
