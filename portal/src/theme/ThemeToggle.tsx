import { useTheme, type ThemeChoice } from "./useTheme";

const OPTIONS: { value: ThemeChoice; label: string }[] = [
  { value: "light", label: "Light" },
  { value: "dark", label: "Dark" },
  { value: "system", label: "System" },
];

export default function ThemeToggle() {
  const { choice, setChoice } = useTheme();
  return (
    <div className="inline-flex rounded-full border border-border bg-surface-2 p-0.5" role="group" aria-label="Theme">
      {OPTIONS.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={choice === o.value}
          onClick={() => setChoice(o.value)}
          className={`rounded-full px-2.5 py-1 text-xs font-medium transition-colors ${
            choice === o.value ? "bg-surface text-text shadow-sm" : "text-muted hover:text-text"
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
