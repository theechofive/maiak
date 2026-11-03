import type { ReactNode } from "react";
import { cx, DotShape, HollowDotShape, PauseShape, TriangleShape } from "./shapes";

export type StatusKind = "up" | "down" | "paused" | "pending";

const DEFAULT_LABEL: Record<StatusKind, string> = {
  up: "Up",
  down: "Down",
  paused: "Paused",
  pending: "Pending",
};

/**
 * Healthy recedes into grey; a filled red block is reserved for down, the only filled colour in
 * the product. `inline` is the unfilled down, for sentences and summaries.
 */
export function StatusIndicator({
  status,
  label,
  inline = false,
}: {
  status: StatusKind;
  /** The word with its duration, such as "Down 12 min". */
  label?: ReactNode;
  inline?: boolean;
}) {
  const text = label ?? DEFAULT_LABEL[status];
  if (status === "down") {
    return inline ? (
      <span className="inline-flex items-center gap-2 font-semibold text-down">
        <TriangleShape />
        {text}
      </span>
    ) : (
      <span className="inline-flex items-center gap-1.5 bg-destructive px-1.5 py-px font-semibold whitespace-nowrap text-destructive-foreground">
        <TriangleShape />
        {text}
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-2 whitespace-nowrap text-muted-foreground">
      {status === "up" && <DotShape className="text-subtle-foreground" />}
      {status === "paused" && <PauseShape />}
      {status === "pending" && <HollowDotShape />}
      {text}
    </span>
  );
}

export type BadgeTone =
  "neutral" | "strong" | "plan" | "inverse" | "warning" | "success" | "danger" | "deleted";

const BADGE_TONES: Record<BadgeTone, string> = {
  neutral: "border-input text-muted-foreground",
  strong: "border-input text-foreground",
  plan: "border-border-faint bg-muted text-foreground",
  inverse: "border-inverse bg-inverse text-inverse-foreground",
  warning: "border-warning-border bg-warning-tint text-warning",
  success: "border-border text-success",
  danger: "border-down-border bg-down-tint text-down",
  deleted: "border-dashed border-input text-subtle-foreground",
};

/** Mono capitals, square. A toned badge also needs a shape or a word that carries the meaning. */
export function Badge({
  tone = "neutral",
  icon,
  children,
}: {
  tone?: BadgeTone;
  icon?: ReactNode;
  children: ReactNode;
}) {
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1.5 border px-1.5 py-px font-mono text-micro tracking-[0.08em] whitespace-nowrap uppercase",
        BADGE_TONES[tone],
      )}
    >
      {icon}
      {children}
    </span>
  );
}
