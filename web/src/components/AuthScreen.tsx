import type { ReactNode } from "react";
import { Button } from "./ui";

/**
 * The sign-in and registration screens: always dark, a slow beam of light behind one panel. The
 * only place the brand gets room; the form stays the clear focus.
 */
export default function AuthScreen({
  children,
  footer,
}: {
  children: ReactNode;
  footer?: ReactNode;
}) {
  return (
    <div className="theme-dark relative isolate flex min-h-dvh flex-col overflow-hidden bg-field text-foreground">
      <div aria-hidden="true" className="maiak-beam-glow" />
      <div aria-hidden="true" className="maiak-beam" />
      <header className="relative z-10 flex h-[52px] items-center px-4 sm:h-[72px] sm:px-8">
        <span className="font-mono text-caption font-semibold tracking-[0.18em]">MAIAK</span>
      </header>
      <div className="relative z-10 flex flex-1 flex-col pt-[72px] sm:items-center sm:justify-center sm:px-8 sm:pt-0 sm:pb-[72px]">
        <main className="flex w-full flex-1 flex-col border-t border-border bg-background sm:max-w-[400px] sm:flex-none sm:border">
          <div className="flex flex-col gap-[18px] px-4 pt-7 pb-8 sm:px-10 sm:pt-9 sm:pb-10">
            {children}
          </div>
          {footer && (
            <p className="flex flex-wrap items-center justify-center gap-1.5 border-t border-border-faint px-4 py-3.5 text-muted-foreground sm:px-10">
              {footer}
            </p>
          )}
        </main>
      </div>
    </div>
  );
}

export function AuthHeading({ children }: { children: ReactNode }) {
  return (
    <h1 className="mb-1.5 text-headline leading-[1.15] font-semibold tracking-[-0.02em] sm:text-[26px]">
      {children}
    </h1>
  );
}

/** Show or hide the password; it controls the field(s) whose ids it is given. */
export function ShowPassword({
  shown,
  controls,
  plural = false,
  onToggle,
}: {
  shown: boolean;
  controls: string;
  plural?: boolean;
  onToggle: () => void;
}) {
  return (
    <Button
      variant="ghost"
      size="small"
      aria-label={`${shown ? "Hide" : "Show"} ${plural ? "passwords" : "password"}`}
      aria-controls={controls}
      onClick={onToggle}
    >
      {shown ? "Hide" : "Show"}
    </Button>
  );
}

export const authLink =
  "font-medium text-foreground underline decoration-input underline-offset-3 hover:decoration-foreground";
