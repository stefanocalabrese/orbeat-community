import type { HTMLAttributes } from "react";

export function Card({ className = "", ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={`rounded-xl border border-border bg-surface shadow-sm ${className}`} {...rest} />;
}

export function Panel({ className = "", ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={`overflow-hidden rounded-xl border border-border bg-surface shadow-sm ${className}`} {...rest} />;
}
