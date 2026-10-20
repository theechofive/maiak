import {
  daysUntil,
  describeErrorKind,
  formatAgo,
  formatDateTime,
  formatDuration,
  formatMs,
  formatPercent,
  formatSince,
  formatTime,
} from "./format";

// vite.config.ts pins TZ=UTC for tests, so "local time" is UTC here.
const now = Date.parse("2026-10-20T14:30:00Z");

describe("formatDuration", () => {
  it.each([
    [0, "0 s"],
    [45, "45 s"],
    [60, "1 min"],
    [125, "2 min"],
    [720, "12 min"],
    [3_600, "1 h"],
    [5_400, "1 h 30 min"],
    [86_400, "1 d"],
    [97_200, "1 d 3 h"],
    [-5, "0 s"],
    [59.9, "59 s"],
  ])("formats %s seconds as %s", (input, expected) => {
    expect(formatDuration(input)).toBe(expected);
  });

  it("returns 'ongoing' when there is no end", () => {
    expect(formatDuration(null)).toBe("ongoing");
    expect(formatDuration(undefined)).toBe("ongoing");
  });
});

describe("formatAgo", () => {
  it.each([
    ["2026-10-20T14:29:20Z", "40 s ago"],
    ["2026-10-20T14:18:00Z", "12 min ago"],
    ["2026-10-20T11:00:00Z", "3 h ago"],
    ["2026-10-18T14:30:00Z", "2 d ago"],
    ["2026-10-20T14:31:00Z", "0 s ago"],
  ])("formats %s as %s", (iso, expected) => {
    expect(formatAgo(iso, now)).toBe(expected);
  });

  it("says never without a time", () => {
    expect(formatAgo(null, now)).toBe("never");
  });
});

describe("times in local time", () => {
  it("formats time, date-time and 'since'", () => {
    expect(formatTime("2026-10-20T14:02:59Z")).toBe("14:02");
    expect(formatDateTime("2026-10-18T09:05:00Z")).toBe("Oct 18, 09:05");
    expect(formatSince("2026-10-20T14:02:00Z", now)).toBe("14:02");
    expect(formatSince("2026-10-19T14:02:00Z", now)).toBe("Oct 19, 14:02");
  });

  it("counts whole days until a time", () => {
    expect(daysUntil("2026-11-03T14:30:00Z", now)).toBe(14);
    expect(daysUntil("2026-11-03T14:29:00Z", now)).toBe(13);
    expect(daysUntil("2026-10-19T14:30:00Z", now)).toBe(-1);
  });
});

describe("numbers", () => {
  it.each([
    [null, "—"],
    [100, "100%"],
    [99.954, "99.95%"],
    [0, "0%"],
  ])("formats percent %s as %s", (input, expected) => {
    expect(formatPercent(input)).toBe(expected);
  });

  it.each([
    [null, "—"],
    [245.4, "245 ms"],
  ])("formats ms %s as %s", (input, expected) => {
    expect(formatMs(input)).toBe(expected);
  });

  it("describes error kinds and keeps unknown ones", () => {
    expect(describeErrorKind("timeout")).toBe("Timeout");
    expect(describeErrorKind("something_new")).toBe("something_new");
    expect(describeErrorKind(null)).toBe("—");
  });
});
