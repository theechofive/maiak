import { describe, expect, it } from "vitest";
import { memberLabel } from "./members";

describe("memberLabel", () => {
  it("prefers the login when the API shows it", () => {
    expect(memberLabel({ login: "a@example.com", displayName: "Ann" })).toBe("a@example.com");
  });
  it("falls back to the display name for members and viewers", () => {
    expect(memberLabel({ login: null, displayName: "Ann" })).toBe("Ann");
  });
  it("never shows an empty name", () => {
    expect(memberLabel({ login: null, displayName: "  " })).toBe("A member");
  });
});
