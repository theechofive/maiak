import { describe, expect, it } from "vitest";
import { memberLabel } from "./members";

const id = "0192b9c4-7c5e-7000-8000-00000000a1f3";

describe("memberLabel", () => {
  it("prefers the login when the API shows it", () => {
    expect(memberLabel({ userId: id, login: "a@example.com", displayName: "Ann" })).toBe(
      "a@example.com",
    );
  });
  it("falls back to the display name for members and viewers", () => {
    expect(memberLabel({ userId: id, login: null, displayName: "Ann" })).toBe("Ann");
  });
  it("uses a stable label from the user id when there is no name, never an email", () => {
    expect(memberLabel({ userId: id, login: null, displayName: "  " })).toBe("Member a1f3");
    expect(memberLabel({ userId: id, login: null, displayName: "" })).toBe(
      memberLabel({ userId: id, login: null, displayName: "" }),
    );
  });
});
