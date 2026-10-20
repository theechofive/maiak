import { useId } from "react";
import type { MonitorStats } from "../api/endpoints";
import { areaPoints, buildChart, CHART, linePoints } from "../lib/chart";
import { formatDateTime, formatMs } from "../lib/format";

const rangeNames = { "24h": "24 hours", "7d": "7 days", "30d": "30 days" } as const;

export default function ResponseChart({ stats }: { stats: MonitorStats }) {
  const id = useId();
  const model = buildChart(stats.series, stats.range);
  const { plot } = model;
  const first = stats.series[0];
  const last = stats.series[stats.series.length - 1];
  const description =
    first && last
      ? `Average response time per interval from ${formatDateTime(first.t)} to ${formatDateTime(last.t)}. ` +
        `Average ${formatMs(stats.avgMs)}, p95 ${formatMs(stats.p95Ms)}. Gaps mean no successful checks.`
      : "No checks in this range.";

  return (
    <svg
      className="chart"
      viewBox={`0 0 ${CHART.width} ${CHART.height}`}
      role="img"
      aria-labelledby={`${id}-title ${id}-desc`}
    >
      <title id={`${id}-title`}>Response time, last {rangeNames[stats.range]}</title>
      <desc id={`${id}-desc`}>{description}</desc>
      {model.yTicks.map((t) => (
        <g key={`y${t.label}`}>
          <line className="grid-line" x1={plot.left} x2={plot.right} y1={t.pos} y2={t.pos} />
          <text className="axis-label" x={plot.left - 6} y={t.pos} textAnchor="end" dy="0.35em">
            {t.label}
          </text>
        </g>
      ))}
      {model.xTicks.map((t, i) => (
        <text
          key={`x${i}`}
          className="axis-label"
          x={t.pos}
          y={plot.bottom + 18}
          textAnchor={
            model.xTicks.length === 1
              ? "middle"
              : i === 0
                ? "start"
                : i === model.xTicks.length - 1
                  ? "end"
                  : "middle"
          }
        >
          {t.label}
        </text>
      ))}
      {model.segments.map((seg, i) =>
        seg.length === 1 ? (
          <circle key={i} className="chart-line" cx={seg[0]!.x} cy={seg[0]!.y} r={2} />
        ) : (
          <g key={i}>
            <polygon className="chart-area" points={areaPoints(seg, plot.bottom)} />
            <polyline className="chart-line" points={linePoints(seg)} />
          </g>
        ),
      )}
    </svg>
  );
}
