import type { ComponentProps } from "react";
import { cx, Spinner } from "./shapes";

export type ButtonVariant =
  | "primary"
  | "secondary"
  | "ghost"
  | "ghost-destructive"
  | "destructive"
  /** Words inside a sentence or a one-line notice: no box, no height. */
  | "link";
export type ButtonSize = "default" | "small" | "square";

const VARIANTS: Record<ButtonVariant, string> = {
  primary:
    "border-primary bg-primary text-primary-foreground hover:border-primary-hover hover:bg-primary-hover active:bg-primary-active",
  secondary:
    "border-input bg-transparent text-foreground hover:border-input-hover hover:bg-muted active:bg-border-faint",
  ghost: "border-transparent bg-transparent text-foreground hover:bg-muted active:bg-border-faint",
  // A destructive action that asks for confirmation next: red words, no red block.
  "ghost-destructive":
    "border-transparent bg-transparent text-down hover:bg-down-tint active:bg-down-tint",
  link: "",
  destructive:
    "border-destructive bg-destructive text-destructive-foreground hover:border-destructive-hover hover:bg-destructive-hover active:bg-destructive-active",
};

// Touch targets grow to 44 px and 36 px on phones.
const SIZES: Record<ButtonSize, string> = {
  default: "h-11 px-4 text-body sm:h-9",
  small: "h-9 px-2.5 text-caption sm:h-7",
  // A one- or two-character button, such as a page number.
  square: "size-9 text-caption sm:size-7",
};

const LINK =
  "inline cursor-pointer font-semibold underline decoration-input underline-offset-3 hover:decoration-current";

const UNAVAILABLE =
  "disabled:cursor-not-allowed disabled:border-border-faint disabled:bg-muted disabled:text-subtle-foreground aria-disabled:cursor-not-allowed aria-disabled:border-border-faint aria-disabled:bg-muted aria-disabled:text-subtle-foreground";

function baseClass(variant: ButtonVariant, size: ButtonSize) {
  if (variant === "link") return LINK;
  return cx(
    "box-border inline-flex cursor-pointer items-center justify-center gap-2 border font-medium whitespace-nowrap transition-colors",
    VARIANTS[variant],
    SIZES[size],
  );
}

/** The classes of a button, for a router Link that must look like one. */
export function buttonClass(variant: ButtonVariant = "primary", size: ButtonSize = "default") {
  return cx(baseClass(variant, size), UNAVAILABLE);
}

export function Button({
  variant = "primary",
  size = "default",
  busy = false,
  mono = false,
  className,
  disabled,
  children,
  ...props
}: ComponentProps<"button"> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
  /** The action is running: shows a spinner and blocks a second click. */
  busy?: boolean;
  /** Machine values as labels: durations, page numbers. */
  mono?: boolean;
}) {
  return (
    <button
      type="button"
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      className={cx(
        // A running button keeps its colours, so it does not flash grey.
        busy
          ? cx(baseClass(variant, size), "cursor-progress opacity-70")
          : buttonClass(variant, size),
        mono && "font-mono",
        className,
      )}
      {...props}
    >
      {busy && <Spinner />}
      {children}
    </button>
  );
}
