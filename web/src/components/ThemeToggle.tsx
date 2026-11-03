import type { ReactNode } from "react";
import { useTheme, type Theme } from "../lib/theme";
import { Button } from "./ui";

/**
 * Light and dark in the top bar. A pressed icon is the saved choice; pressing it again returns to
 * the system setting, which Settings → Appearance also offers by name.
 */
export default function ThemeToggle() {
  const [theme, setTheme] = useTheme();
  const option = (value: Exclude<Theme, "system">, label: string, icon: ReactNode) => {
    const on = theme === value;
    return (
      <Button
        variant={on ? "secondary" : "ghost"}
        size="square"
        aria-label={label}
        aria-pressed={on}
        title={on ? `${label} (press again to follow the system)` : label}
        onClick={() => setTheme(on ? "system" : value)}
      >
        {icon}
      </Button>
    );
  };
  return (
    <div role="group" aria-label="Theme" className="flex items-center gap-1">
      {option("light", "Light theme", <SunIcon />)}
      {option("dark", "Dark theme", <MoonIcon />)}
    </div>
  );
}

function SunIcon() {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 16 16"
      className="size-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
    >
      <circle cx="8" cy="8" r="3" />
      <path
        d="M8 1v1.5M8 13.5V15M1 8h1.5M13.5 8H15M3.05 3.05l1.06 1.06M11.89 11.89l1.06 1.06M3.05 12.95l1.06-1.06M11.89 4.11l1.06-1.06"
        strokeLinecap="square"
      />
    </svg>
  );
}

function MoonIcon() {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 16 16"
      className="size-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
    >
      <path d="M13.5 9.6A5.75 5.75 0 0 1 6.4 2.5a5.75 5.75 0 1 0 7.1 7.1Z" strokeLinejoin="round" />
    </svg>
  );
}
