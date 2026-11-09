import { describe, expect, it } from "vitest";
import { isUnlimited, UNLIMITED } from "./limits";
import { countOf } from "./usage";

describe("unlimited limits", () => {
  it("treats the largest limit as no quota and never prints it", () => {
    expect(isUnlimited(UNLIMITED)).toBe(true);
    expect(isUnlimited(1000)).toBe(false);
    expect(countOf(UNLIMITED, "monitor", "monitors")).toBe("unlimited monitors");
    expect(countOf(3, "monitor", "monitors")).toBe("3 monitors");
    expect(countOf(1, "monitor", "monitors")).toBe("1 monitor");
  });
});
