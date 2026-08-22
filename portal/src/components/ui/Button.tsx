import type { ButtonHTMLAttributes } from "react";

type Variant = "primary" | "ghost" | "approve" | "danger";
const variantCls: Record<Variant, string> = {
  primary: "bg-accent text-white shadow-sm hover:brightness-110",
  ghost: "border border-border-strong text-muted hover:bg-surface-2 hover:text-text",
  approve: "bg-ok-solid text-white hover:brightness-110",
  danger: "border border-border-strong text-muted hover:bg-danger-weak hover:text-danger hover:border-danger",
};

export function Button({ variant = "primary", className = "", ...rest }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant }) {
  return (
    <button
      className={`inline-flex items-center justify-center rounded-md px-3.5 py-2 text-sm font-medium transition-[background,filter,color,border-color] disabled:opacity-50 ${variantCls[variant]} ${className}`}
      {...rest}
    />
  );
}
