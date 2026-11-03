import { useState, type KeyboardEvent, type ReactNode } from "react";
import type { LatencyPoint } from "../../api/endpoints";
import { describeSegment, ribbonScale, ribbonSegment } from "../../lib/ribbon";
import { cx } from "./shapes";

/**
 * One monitor's history as one mark: height is the response time relative to the slowest
 * bucket of the range, tone is the share of failed checks, a break means no checks ran. With `label` it can be inspected by hover or arrow keys.
 */
export function SignalRibbon({
  points,
  label,
  heightPx = 120,
  axis,
  below,
}: {
  points: LatencyPoint[];
  /** The accessible name; a ribbon without it is decorative (for example in a table row). */
  label?: string;
  heightPx?: number;
  /** Time labels under the ribbon. */
  axis?: ReactNode[];
  /** Drawn between the ribbon and its time labels, on the same time axis (the incident lane). */
  below?: ReactNode;
}) {
  const [active, setActive] = useState<number | null>(null);
  const interactive = label !== undefined;
  const last = points.length - 1;
  const scale = ribbonScale(points);

  function onKeyDown(e: KeyboardEvent) {
    const current = active ?? last;
    const next = {
      ArrowLeft: Math.max(0, current - 1),
      ArrowRight: Math.min(last, current + 1),
      Home: 0,
      End: last,
    }[e.key];
    if (next === undefined) return;
    e.preventDefault();
    setActive(next);
  }

  const activePoint = active === null ? undefined : points[active];
  const tip = activePoint && describeSegment(activePoint);
  const tipAt = active === null ? 0 : (active + 0.5) / points.length;

  return (
    <div className={cx("relative min-w-0", interactive && "pt-14")}>
      {tip && (
        <div
          role="status"
          className={cx(
            "pointer-events-none absolute top-0 z-10 flex flex-col gap-0.5 bg-inverse px-2.5 py-1.5 font-mono text-mono whitespace-nowrap text-inverse-foreground",
            tipAt < 0.1
              ? "-translate-x-[10%]"
              : tipAt > 0.9
                ? "-translate-x-[90%]"
                : "-translate-x-1/2",
          )}
          style={{ left: `${tipAt * 100}%` }}
        >
          <span className="font-medium">{tip.time}</span>
          <span>{tip.detail}</span>
        </div>
      )}
      <div
        {...(interactive
          ? {
              role: "group",
              "aria-label": `${label}. Use the arrow keys to inspect.`,
              tabIndex: 0,
              onKeyDown,
              onFocus: () => setActive((a) => a ?? last),
              onBlur: () => setActive(null),
              onMouseLeave: () => setActive(null),
            }
          : { "aria-hidden": true })}
        className={cx(
          "flex items-end gap-px border-b",
          interactive ? "border-input outline-offset-4" : "border-border",
        )}
        style={{ height: heightPx }}
      >
        {points.map((point, i) => {
          const segment = ribbonSegment(point, scale);
          const on = active === i;
          return (
            <div
              key={point.t}
              onMouseEnter={interactive ? () => setActive(i) : undefined}
              className={cx(
                "flex h-full min-w-0 flex-1 items-end",
                interactive && "cursor-crosshair",
                on && "bg-muted",
              )}
            >
              <div
                className="w-full"
                style={{
                  height: `${segment.heightPct}%`,
                  background: on && segment.ran ? "var(--foreground)" : segment.color,
                }}
              />
            </div>
          );
        })}
      </div>
      {below}
      {axis && (
        <div className="flex justify-between pt-1.5 font-mono text-micro text-muted-foreground">
          {axis.map((tick, i) => (
            <span key={i}>{tick}</span>
          ))}
        </div>
      )}
    </div>
  );
}

/** No data yet: a dashed line where the ribbon will be. */
export function RibbonPlaceholder({ children }: { children?: ReactNode }) {
  return (
    <div className="flex h-[18px] items-end border-b border-dashed border-input pb-[3px] text-mono text-subtle-foreground">
      {children}
    </div>
  );
}

/** The data refreshed: change `runKey` to play one slow, quiet sweep. Hidden under reduced motion. */
export function Sweep({ runKey }: { runKey: number }) {
  return runKey > 0 ? <div key={runKey} aria-hidden="true" className="maiak-sweep" /> : null;
}
