import { formatDuration } from "./format";

describe("formatDuration", () => {
  it.each([
    [0, "0s"],
    [45, "45s"],
    [60, "1m"],
    [125, "2m 5s"],
    [3_600, "1h"],
    [5_400, "1h 30m"],
    [86_400, "1d"],
    [97_200, "1d 3h"],
    [-5, "0s"],
  ])("formats %s seconds as %s", (input, expected) => {
    expect(formatDuration(input)).toBe(expected);
  });

  it("returns 'ongoing' when there is no end", () => {
    expect(formatDuration(null)).toBe("ongoing");
    expect(formatDuration(undefined)).toBe("ongoing");
  });
});
