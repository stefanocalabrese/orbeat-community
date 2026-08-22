import { useCallback, useEffect, useState } from "react";

export type ThemeChoice = "light" | "dark" | "system";
const KEY = "orbeat-theme";

function read(): ThemeChoice {
  // Storage access throws when blocked (enterprise policy / private mode /
  // disabled cookies). Mirror the pre-paint bootstrap (public/theme-init.js):
  // swallow the error and fall back to the system default rather than crash to
  // a white page at startup.
  try {
    const v = localStorage.getItem(KEY);
    return v === "light" || v === "dark" ? v : "system";
  } catch {
    return "system";
  }
}

function apply(choice: ThemeChoice) {
  const el = document.documentElement;
  if (choice === "system") el.removeAttribute("data-theme");
  else el.setAttribute("data-theme", choice);
}

export function useTheme() {
  const [choice, setChoiceState] = useState<ThemeChoice>(read);

  useEffect(() => {
    apply(choice);
  }, [choice]);

  const setChoice = useCallback((next: ThemeChoice) => {
    // Persist best-effort: a blocked store must not stop the in-session theme
    // change (apply() still runs off `choice`), so swallow write failures too.
    try {
      if (next === "system") localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, next);
    } catch {
      // storage unavailable — choice is still applied for this session
    }
    setChoiceState(next);
  }, []);

  return { choice, setChoice };
}
