import { areaPoints, buildChart, linePoints, niceCeiling } from "./chart";

const size = { width: 200, height: 120, top: 10, right: 10, bottom: 10, left: 10 };
// Plot area: x 10..190, y 10..110 (baseline 110).

describe("niceCeiling", () => {
  it.each([
    [0, 100],
    [-1, 100],
    [1, 1],
    [3, 5],
    [7, 10],
    [120, 200],
    [480, 500],
    [501, 1000],
    [0.3, 0.5],
  ])("%s -> %s", (input, expected) => {
    expect(niceCeiling(input)).toBe(expected);
  });
});

describe("buildChart", () => {
  const t = (h: number) => `2026-10-20T${String(h).padStart(2, "0")}:00:00Z`;

  it("scales time to x and latency to y from zero to a nice maximum", () => {
    const model = buildChart(
      [
        { t: t(0), avgMs: 100, okRatio: 1 },
        { t: t(1), avgMs: 200, okRatio: 1 },
        { t: t(2), avgMs: 50, okRatio: 0.5 },
      ],
      "24h",
      size,
    );
    expect(model.yMax).toBe(200);
    expect(model.segments).toEqual([
      [
        { x: 10, y: 60 },
        { x: 100, y: 10 },
        { x: 190, y: 85 },
      ],
    ]);
    expect(model.yTicks.map((tick) => tick.label)).toEqual(["0 ms", "100 ms", "200 ms"]);
    expect(model.yTicks.map((tick) => tick.pos)).toEqual([110, 60, 10]);
  });

  it("breaks the line where okRatio or avgMs is null", () => {
    const model = buildChart(
      [
        { t: t(0), avgMs: 100, okRatio: 1 },
        { t: t(1), avgMs: 100, okRatio: 1 },
        { t: t(2), avgMs: null, okRatio: null },
        { t: t(3), avgMs: 100, okRatio: 1 },
        { t: t(4), avgMs: null, okRatio: 0 },
        { t: t(5), avgMs: 100, okRatio: 1 },
      ],
      "24h",
      size,
    );
    expect(model.segments.map((s) => s.length)).toEqual([2, 1, 1]);
  });

  it("labels the x axis in local time for 24 h and with dates for longer ranges", () => {
    const series = [
      { t: "2026-10-13T00:00:00Z", avgMs: 1, okRatio: 1 },
      { t: "2026-10-20T00:00:00Z", avgMs: 1, okRatio: 1 },
    ];
    expect(buildChart(series, "7d", size).xTicks.map((x) => x.label)).toEqual([
      "Oct 13",
      "Oct 14",
      "Oct 16",
      "Oct 18",
      "Oct 20",
    ]);
    expect(buildChart(series.slice(0, 1), "24h", size).xTicks[0]?.label).toBe("00:00");
  });

  it("handles an empty series", () => {
    const model = buildChart([], "30d", size);
    expect(model.segments).toEqual([]);
    expect(model.xTicks).toEqual([]);
    expect(model.yMax).toBe(100);
  });

  it("closes the area down to the baseline", () => {
    const points = [
      { x: 10, y: 60 },
      { x: 100, y: 10 },
    ];
    expect(linePoints(points)).toBe("10,60 100,10");
    expect(areaPoints(points, 110)).toBe("10,110 10,60 100,10 100,110");
  });
});
