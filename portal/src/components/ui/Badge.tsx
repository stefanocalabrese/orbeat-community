import type { ReactNode } from "react";

type State = "draft" | "pending" | "approved" | "rejected";
const stateCls: Record<State, string> = {
  draft: "text-muted bg-surface-2",
  pending: "text-warn bg-warn-weak",
  approved: "text-ok bg-ok-weak",
  rejected: "text-danger bg-danger-weak",
};

export function StateBadge({ state }: { state: State }) {
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-semibold ${stateCls[state]}`}>
      <span className="h-1.5 w-1.5 rounded-full bg-current" />
      {state}
    </span>
  );
}

const pillCls = {
  neutral: "text-muted bg-surface-2",
  accent: "text-accent-ink bg-accent-weak",
  blue: "text-blue-ink bg-blue-weak",
  ok: "text-ok bg-ok-weak",
  info: "text-info bg-info-weak",
} as const;

export function Pill({ variant = "neutral", dot = false, children }: { variant?: keyof typeof pillCls; dot?: boolean; children: ReactNode }) {
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-semibold ${pillCls[variant]}`}>
      {dot && <span className="h-1.5 w-1.5 rounded-full bg-current" />}
      {children}
    </span>
  );
}

const chipCls = {
  neutral: "text-muted bg-surface-2",
  subagent: "text-accent-ink bg-accent-weak",
  skill: "text-info bg-info-weak",
  rule: "text-blue-ink bg-blue-weak",
} as const;

export function Chip({ variant = "neutral", children }: { variant?: keyof typeof chipCls; children: ReactNode }) {
  return <span className={`rounded-md px-2 py-0.5 text-xs font-semibold ${chipCls[variant]}`}>{children}</span>;
}
