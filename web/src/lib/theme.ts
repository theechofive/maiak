import { useSyncExternalStore } from "react";

export type Theme = "system" | "light" | "dark";

/** Shared with public/theme.js, which applies the choice before the app loads. */
const KEY = "maiak-theme";

// Every control that shows the theme (the top bar, Settings) re-renders on a change.
const listeners = new Set<() => void>();
// The choice when storage is blocked, so the controls still agree until a reload.
let fallback: Theme = "system";

export function readTheme(): Theme {
  try {
    const saved = localStorage.getItem(KEY);
    return saved === "light" || saved === "dark" ? saved : "system";
  } catch {
    return fallback;
  }
}

/** Saves the choice for this browser and applies it at once; "system" follows the device. */
export function applyTheme(theme: Theme): void {
  const root = document.documentElement;
  root.classList.remove("theme-light", "theme-dark");
  if (theme !== "system") root.classList.add(`theme-${theme}`);
  fallback = theme;
  try {
    if (theme === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, theme);
  } catch {
    // Storage blocked: the choice lasts until the page is reloaded.
  }
  listeners.forEach((notify) => notify());
}

function subscribe(notify: () => void) {
  listeners.add(notify);
  return () => listeners.delete(notify);
}

export function useTheme(): [Theme, (theme: Theme) => void] {
  return [useSyncExternalStore(subscribe, readTheme), applyTheme];
}
