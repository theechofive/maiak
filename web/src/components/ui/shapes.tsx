export function cx(...classes: (string | false | null | undefined)[]): string {
  return classes.filter(Boolean).join(" ");
}

/** Status is never colour alone: each state has one of these shapes next to its word. */
export function DotShape({
  className,
  size = "default",
}: {
  /** Colour and margins only: sizes come from `size`, since utilities are not merged. */
  className?: string;
  size?: "default" | "large";
}) {
  return (
    <span
      aria-hidden="true"
      className={cx(
        "flex-none rounded-full bg-current",
        size === "large" ? "size-[7px]" : "size-1.5",
        className,
      )}
    />
  );
}

export function HollowDotShape() {
  return (
    <span
      aria-hidden="true"
      className="box-border size-[7px] flex-none rounded-full border-[1.5px] border-current"
    />
  );
}

const TRIANGLE = { small: "h-[7px] w-2", default: "h-2 w-[9px]", large: "h-3 w-[13px]" };

export function TriangleShape({
  className,
  size = "default",
}: {
  /** Colour and margins only: sizes come from `size`, since utilities are not merged. */
  className?: string;
  size?: keyof typeof TRIANGLE;
}) {
  return (
    <span
      aria-hidden="true"
      className={cx("flex-none bg-current shape-triangle", TRIANGLE[size], className)}
    />
  );
}

export function DiamondShape({ className }: { className?: string }) {
  return (
    <span aria-hidden="true" className={cx("size-1.5 flex-none rotate-45 bg-current", className)} />
  );
}

export function PauseShape() {
  return (
    <span
      aria-hidden="true"
      className="box-border h-2 w-[7px] flex-none border-x-[2.5px] border-current"
    />
  );
}

export function Spinner() {
  return (
    <span
      aria-hidden="true"
      className="box-border size-3 flex-none animate-spin rounded-full border-[1.5px] border-current border-r-transparent"
    />
  );
}
