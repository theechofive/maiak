import type { LatencyPoint, StatsRange } from "../api/endpoints";
import { formatDate, formatTime } from "./format";

export interface Point {
  x: number;
  y: number;
}

export interface Tick {
  pos: number;
  label: string;
}

export interface ChartModel {
  /** Runs of consecutive points with data; a bucket without data breaks the line. */
  segments: Point[][];
  xTicks: Tick[];
  yTicks: Tick[];
  yMax: number;
  plot: { left: number; right: number; top: number; bottom: number };
}

export const CHART = { width: 640, height: 220, top: 10, right: 16, bottom: 28, left: 56 };

/** The smallest 1, 2 or 5 times a power of ten that is at least `value` (100 for no data). */
export function niceCeiling(value: number): number {
  if (!(value > 0)) return 100;
  const magnitude = 10 ** Math.floor(Math.log10(value));
  for (const step of [1, 2, 5, 10]) {
    if (step * magnitude >= value) return step * magnitude;
  }
  return 10 * magnitude;
}

const round = (n: number) => Math.round(n * 10) / 10;

/** Scales the latency series into SVG coordinates. Labels are in local time. */
export function buildChart(series: LatencyPoint[], range: StatsRange, size = CHART): ChartModel {
  const plot = {
    left: size.left,
    right: size.width - size.right,
    top: size.top,
    bottom: size.height - size.bottom,
  };
  const times = series.map((p) => Date.parse(p.t));
  const t0 = times.length ? Math.min(...times) : 0;
  const t1 = times.length ? Math.max(...times) : 0;
  const span = t1 > t0 ? t1 - t0 : 3_600_000;
  const values = series.flatMap((p) => (p.okRatio != null && p.avgMs != null ? [p.avgMs] : []));
  const yMax = niceCeiling(values.length ? Math.max(...values) : 0);

  const x = (t: number) => round(plot.left + ((t - t0) / span) * (plot.right - plot.left));
  const y = (v: number) => round(plot.bottom - (v / yMax) * (plot.bottom - plot.top));

  const segments: Point[][] = [];
  let current: Point[] = [];
  series.forEach((p, i) => {
    if (p.okRatio == null || p.avgMs == null) {
      if (current.length) segments.push(current);
      current = [];
      return;
    }
    current.push({ x: x(times[i]!), y: y(p.avgMs) });
  });
  if (current.length) segments.push(current);

  const label = range === "24h" ? formatTime : formatDate;
  const xTicks: Tick[] = series.length
    ? [0, 0.25, 0.5, 0.75, 1].map((f) => ({ pos: x(t0 + f * span), label: label(t0 + f * span) }))
    : [];
  const yTicks: Tick[] = [0, yMax / 2, yMax].map((v) => ({ pos: y(v), label: `${v} ms` }));

  return { segments, xTicks, yTicks, yMax, plot };
}

export const linePoints = (points: Point[]) => points.map((p) => `${p.x},${p.y}`).join(" ");

/** Polygon points that close the line down to the baseline. */
export function areaPoints(points: Point[], baseline: number): string {
  if (points.length === 0) return "";
  const first = points[0]!;
  const last = points[points.length - 1]!;
  return `${first.x},${baseline} ${linePoints(points)} ${last.x},${baseline}`;
}
