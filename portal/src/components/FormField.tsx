import type { ReactNode } from "react";

export function FormField({
  label,
  htmlFor,
  children,
  hint,
}: {
  label: string;
  htmlFor: string;
  children: ReactNode;
  hint?: string;
}) {
  return (
    <div>
      <label htmlFor={htmlFor} className="block text-sm font-medium text-text">
        {label}
      </label>
      {children}
      {hint && <p className="mt-1 text-xs text-muted">{hint}</p>}
    </div>
  );
}

export const inputCls =
  "w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm text-text placeholder:text-faint focus-visible:border-accent";
