import type { Incident, LatencyPoint, StatsRange } from "../api/endpoints";
import { formatDate, formatDateTime, formatMs, formatTime } from "./format";

export interface RibbonSegment {
  /** False when no check ran in the bucket: the ribbon shows a break. */
  ran: boolean;
  /** Bar height as a percentage of the ribbon. */
  heightPct: number;
  /** A CSS colour built from the design tokens. */
  color: string;
}

/** At this share of failed checks a bucket counts as down and fills the ribbon. */
const DOWN_SHARE = 0.8;
const MIN_HEIGHT_PCT = 6;

/**
 * The response time that reaches full height: the slowest bucket of the range, leaving out the
 * failed ones (they are full height anyway, and a 10 s timeout would flatten everything else).
 */
export function ribbonScale(points: LatencyPoint[]): number {
  return points.reduce(
    (max, p) =>
      p.okRatio != null && 1 - p.okRatio < DOWN_SHARE && p.avgMs != null
        ? Math.max(max, p.avgMs)
        : max,
    0,
  );
}

export function ribbonColor(failedShare: number): string {
  if (failedShare <= 0) return "var(--ribbon)";
  const mix = Math.round(40 + 60 * failedShare);
  // oklab: a mix in oklch would pass through violet on the way from grey to red.
  return `color-mix(in oklab, var(--ribbon), var(--destructive) ${mix}%)`;
}

/** One bucket, with heights relative to `scaleMs` (see `ribbonScale`). */
export function ribbonSegment(point: LatencyPoint, scaleMs: number): RibbonSegment {
  if (point.okRatio == null) return { ran: false, heightPct: 0, color: "transparent" };
  const failedShare = 1 - point.okRatio;
  const scaled = scaleMs > 0 ? ((point.avgMs ?? 0) / scaleMs) * 100 : 0;
  const heightPct =
    failedShare >= DOWN_SHARE ? 100 : Math.round(Math.max(MIN_HEIGHT_PCT, Math.min(100, scaled)));
  return { ran: true, heightPct, color: ribbonColor(failedShare) };
}

/** The tooltip for one bucket; p95 exists only per range in the API, not per bucket. */
export function describeSegment(point: LatencyPoint): { time: string; detail: string } {
  const time = formatDateTime(point.t);
  if (point.okRatio == null) return { time, detail: "No checks ran" };
  const avg = point.avgMs == null ? "no response" : `avg ${formatMs(point.avgMs)}`;
  return { time, detail: `uptime ${Math.round(point.okRatio * 100)}% · ${avg}` };
}

/** Bucket width per range, as the stats endpoint documents it. */
export const BUCKET_MS: Record<StatsRange, number> = {
  "24h": 15 * 60_000,
  "7d": 3_600_000,
  "30d": 6 * 3_600_000,
};

/** The time span a series covers: from its first bucket to the end of its last one. */
export function seriesSpan(series: LatencyPoint[], range: StatsRange): [number, number] | null {
  const first = series[0];
  const last = series[series.length - 1];
  if (!first || !last) return null;
  return [Date.parse(first.t), Date.parse(last.t) + BUCKET_MS[range]];
}

/** Five labels under the ribbon; the last one is "Now". */
export function axisLabels([start, end]: [number, number], range: StatsRange): string[] {
  const label = range === "24h" ? formatTime : formatDate;
  return [0, 0.25, 0.5, 0.75].map((f) => label(start + f * (end - start))).concat("Now");
}

export interface IncidentSpan {
  id: string;
  /** Start and end as fractions of the span, clipped to 0..1. */
  from: number;
  to: number;
  ongoing: boolean;
}

/** Incidents placed on the ribbon's time axis; those entirely outside the span are dropped. */
export function incidentSpans(
  incidents: Incident[],
  [start, end]: [number, number],
  now: number,
): IncidentSpan[] {
  const width = end - start;
  if (width <= 0) return [];
  const clip = (t: number) => Math.min(1, Math.max(0, (t - start) / width));
  return incidents.flatMap((i) => {
    const s = Date.parse(i.startedAt);
    const e = i.resolvedAt ? Date.parse(i.resolvedAt) : now;
    if (e < start || s > end) return [];
    return [{ id: i.id, from: clip(s), to: clip(e), ongoing: !i.resolvedAt }];
  });
}
