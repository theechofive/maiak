import {
  axisLabels,
  describeSegment,
  incidentSpans,
  ribbonColor,
  ribbonScale,
  ribbonSegment,
  seriesSpan,
} from "./ribbon";

const point = (avgMs: number | null, okRatio: number | null) => ({
  t: "2026-11-02T03:00:00Z",
  avgMs,
  okRatio,
});

describe("ribbonScale", () => {
  it("is the slowest bucket that did not fail", () => {
    expect(ribbonScale([point(80, 1), point(140, 0.9), point(10_000, 0), point(null, null)])).toBe(
      140,
    );
    expect(ribbonScale([point(null, null)])).toBe(0);
  });
});

describe("ribbonSegment", () => {
  it("breaks the ribbon when no check ran", () => {
    expect(ribbonSegment(point(null, null), 200)).toEqual({
      ran: false,
      heightPct: 0,
      color: "transparent",
    });
  });

  it("scales height to the slowest bucket of the range", () => {
    expect(ribbonSegment(point(100, 1), 200).heightPct).toBe(50);
    expect(ribbonSegment(point(200, 1), 200).heightPct).toBe(100);
  });

  it("keeps fast buckets visible", () => {
    expect(ribbonSegment(point(1, 1), 200).heightPct).toBe(6);
    expect(ribbonSegment(point(50, 1), 0).heightPct).toBe(6);
  });

  it("fills the ribbon when most checks failed, even with no response at all", () => {
    expect(ribbonSegment(point(120, 0.2), 200).heightPct).toBe(100);
    expect(ribbonSegment(point(null, 0), 200).heightPct).toBe(100);
  });

  it("uses the minimum height for a partly failed bucket with no response time", () => {
    expect(ribbonSegment(point(null, 0.5), 200).heightPct).toBe(6);
  });
});

describe("ribbonColor", () => {
  it("stays quiet with no failures and mixes towards red with more of them", () => {
    expect(ribbonColor(0)).toBe("var(--ribbon)");
    expect(ribbonColor(0.5)).toContain("var(--destructive) 70%");
    expect(ribbonColor(1)).toContain("var(--destructive) 100%");
  });
});

describe("describeSegment", () => {
  it("explains a bucket without checks", () => {
    expect(describeSegment(point(null, null)).detail).toBe("No checks ran");
  });

  it("reports uptime and average", () => {
    expect(describeSegment(point(120, 0.4)).detail).toBe("uptime 40% · avg 120 ms");
    expect(describeSegment(point(null, 0)).detail).toBe("uptime 0% · no response");
  });
});

describe("seriesSpan and axisLabels", () => {
  it("covers the first bucket to the end of the last one", () => {
    const span = seriesSpan([point(1, 1), { ...point(1, 1), t: "2026-11-02T04:00:00Z" }], "24h");
    expect(span).toEqual([Date.parse("2026-11-02T03:00:00Z"), Date.parse("2026-11-02T04:15:00Z")]);
    expect(seriesSpan([], "24h")).toBeNull();
  });

  it("ends the axis at Now", () => {
    const labels = axisLabels([0, 3_600_000], "24h");
    expect(labels).toHaveLength(5);
    expect(labels[4]).toBe("Now");
  });
});

describe("incidentSpans", () => {
  const start = Date.parse("2026-11-02T00:00:00Z");
  const end = start + 100 * 60_000;
  const at = (min: number) => new Date(start + min * 60_000).toISOString();
  const incident = (id: string, from: number, to: number | null) => ({
    id,
    startedAt: at(from),
    resolvedAt: to === null ? null : at(to),
    reason: "timeout",
  });

  it("places, clips and keeps ongoing incidents open until now", () => {
    expect(
      incidentSpans(
        [incident("a", 10, 20), incident("b", -30, 5), incident("c", 90, null)],
        [start, end],
        start + 95 * 60_000,
      ),
    ).toEqual([
      { id: "a", from: 0.1, to: 0.2, ongoing: false },
      { id: "b", from: 0, to: 0.05, ongoing: false },
      { id: "c", from: 0.9, to: 0.95, ongoing: true },
    ]);
  });

  it("drops incidents outside the span", () => {
    expect(incidentSpans([incident("old", -60, -10)], [start, end], end)).toEqual([]);
  });
});
