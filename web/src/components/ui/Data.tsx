import type { HTMLAttributes, ReactNode, TdHTMLAttributes, ThHTMLAttributes } from "react";
import { Button } from "./Button";
import { cx, DiamondShape } from "./shapes";
import { isUnlimited } from "../../lib/limits";

/** A ruled strip of figures, never a row of cards. */
export function MetricStrip({ children }: { children: ReactNode }) {
  return (
    <div className="grid grid-cols-2 gap-0 border-y border-border sm:auto-cols-fr sm:grid-flow-col sm:grid-cols-none">
      {children}
    </div>
  );
}

/** Colour appears only when the figure needs action. */
export function Metric({
  label,
  value,
  note,
  tone = "default",
}: {
  label: ReactNode;
  value: ReactNode;
  note?: ReactNode;
  tone?: "default" | "down" | "warning";
}) {
  const warn = tone === "warning";
  return (
    <div
      className={cx(
        "flex min-w-0 flex-col gap-1.5 border-l border-border-faint px-4 py-3.5 first:border-l-0",
        warn && "bg-warning-tint",
      )}
    >
      <span className={cx("eyebrow flex items-center gap-1.5", warn && "text-warning")}>
        {warn && <DiamondShape />}
        {label}
      </span>
      <span
        className={cx(
          "font-mono text-headline font-medium tracking-[-0.02em]",
          tone === "down" && "text-down",
        )}
      >
        {value}
      </span>
      {note && (
        <span className={cx("text-caption", warn ? "text-warning" : "text-muted-foreground")}>
          {note}
        </span>
      )}
    </div>
  );
}

/** Usage against a limit: quiet until 80 %, then it says how much is left. */
export function Meter({ label, used, limit }: { label: string; used: number; limit: number }) {
  // No quota (the `community` plan, ADR-0105): the count without a bar or a warning.
  if (isUnlimited(limit)) {
    return (
      <div className="grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-4 gap-y-2.5 py-3.5">
        <span className="text-[14px]">{label}</span>
        <span className="font-mono">
          <span className="font-medium">{used}</span>
          <span className="text-muted-foreground"> / Unlimited</span>
        </span>
      </div>
    );
  }
  const left = limit - used;
  const full = left <= 0;
  const near = !full && limit > 0 && used / limit >= 0.8;
  const pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 100;
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-4 gap-y-2.5 py-3.5">
      <span className="text-[14px]">{label}</span>
      <span className="flex items-baseline gap-3">
        {(full || near) && (
          <span className="inline-flex items-center gap-1.5 text-caption font-medium text-warning">
            <DiamondShape />
            {full ? "Limit reached" : `${left} left`}
          </span>
        )}
        <span className="font-mono">
          <span className="font-medium">{used}</span>
          <span className="text-muted-foreground"> / {limit}</span>
        </span>
      </span>
      <div
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={limit}
        aria-valuenow={used}
        className="col-span-full h-0.5 bg-border-faint"
      >
        <div
          className={cx("h-full", full || near ? "bg-warning" : "bg-ribbon")}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}

export function Table({ children, className }: { children: ReactNode; className?: string }) {
  return (
    // relative: absolutely positioned content (an sr-only header) must scroll with the table,
    // not widen the page.
    <div className={cx("relative overflow-x-auto border border-border", className)}>
      <table className="w-full border-collapse">{children}</table>
    </div>
  );
}

export function Th({
  numeric = false,
  className,
  children,
  ...props
}: ThHTMLAttributes<HTMLTableCellElement> & { numeric?: boolean }) {
  return (
    <th
      scope="col"
      className={cx(
        "eyebrow border-b border-border px-4 py-2 font-normal whitespace-nowrap",
        numeric ? "text-right" : "text-left",
        className,
      )}
      {...props}
    >
      {children}
    </th>
  );
}

export function Td({
  numeric = false,
  className,
  children,
  ...props
}: TdHTMLAttributes<HTMLTableCellElement> & { numeric?: boolean }) {
  return (
    <td
      className={cx(
        "border-b border-[inherit] px-4 py-2.5",
        numeric && "text-right font-mono text-mono whitespace-nowrap",
        className,
      )}
      {...props}
    >
      {children}
    </td>
  );
}

/** Rows hover to muted and focus with an inset ring. A row with `tone="down"` gets the red tint. */
export function Tr({
  tone = "default",
  interactive = false,
  className,
  children,
  ...props
}: HTMLAttributes<HTMLTableRowElement> & {
  tone?: "default" | "down" | "warning" | "deleted";
  interactive?: boolean;
}) {
  return (
    <tr
      className={cx(
        "border-border-faint last:[&>td]:border-b-0",
        interactive && "cursor-pointer hover:bg-muted focus-visible:-outline-offset-2",
        tone === "down" && "border-down-border bg-down-tint hover:bg-down-tint",
        tone === "warning" && "bg-warning-tint hover:bg-warning-tint",
        tone === "deleted" && "text-subtle-foreground",
        className,
      )}
      {...props}
    >
      {children}
    </tr>
  );
}

export function Pagination({
  page,
  pageCount,
  label,
  onPage,
}: {
  page: number;
  pageCount: number;
  /** Such as "1–25 of 40". */
  label: ReactNode;
  onPage: (page: number) => void;
}) {
  // shortcut: numbers only up to 7 pages, beyond that Previous and Next; add ellipses when the
  // admin list gets there.
  const numbers = pageCount <= 7 ? Array.from({ length: pageCount }, (_, i) => i + 1) : [];
  return (
    <nav
      aria-label="Pages"
      className="flex flex-wrap items-center justify-between gap-2 px-4 py-2 font-mono text-mono text-muted-foreground"
    >
      <span>{label}</span>
      <div className="flex gap-1">
        <Button
          variant="secondary"
          size="small"
          disabled={page <= 1}
          onClick={() => onPage(page - 1)}
        >
          Previous
        </Button>
        {numbers.map((n) => (
          <Button
            key={n}
            variant={n === page ? "primary" : "secondary"}
            size="square"
            mono
            aria-current={n === page ? "page" : undefined}
            onClick={() => onPage(n)}
          >
            {n}
          </Button>
        ))}
        <Button
          variant="secondary"
          size="small"
          disabled={page >= pageCount}
          onClick={() => onPage(page + 1)}
        >
          Next
        </Button>
      </div>
    </nav>
  );
}

/** Elevation 1 is a hairline: no shadow at rest. */
export function Card({
  title,
  aside,
  children,
}: {
  title?: ReactNode;
  aside?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="flex flex-col border border-border bg-card">
      {title && (
        <header className="flex items-center gap-2.5 border-b border-border-faint px-4 py-3">
          <h2 className="text-lead font-semibold">{title}</h2>
          {aside}
        </header>
      )}
      <div className="flex flex-col gap-3 p-4">{children}</div>
    </section>
  );
}

/** A static shape, no shimmer: the refresh sweep is the only ambient motion. */
export function Skeleton({ className }: { className?: string }) {
  return <span aria-hidden="true" className={cx("block h-2.5 bg-muted", className)} />;
}

export function RibbonSkeleton() {
  return (
    <span
      aria-hidden="true"
      className="block h-3.5 bg-[repeating-linear-gradient(90deg,var(--border-faint)_0_3px,transparent_3px_4px)]"
    />
  );
}
