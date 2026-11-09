import {
  codePoints,
  displayNameError,
  emailError,
  normalizeEmail,
  validateRegisterForm,
} from "./registerForm";

describe("registration policy (mirrors the server)", () => {
  it.each(["owner@example.com", "  User+Tag@Example.COM ", "a_b-c.d@sub.example.co.uk", "a@b.c"])(
    "accepts %j",
    (email) => {
      expect(emailError(email)).toBeUndefined();
    },
  );

  it.each([
    ["", "Enter your email address"],
    ["ab", "Use 3-254 characters"],
    [`${"a".repeat(243)}@example.com`, "Use 3-254 characters"],
    ["jörg@example.com", "Use only letters a-z, digits and . _ + - @"],
    ["a b@example.com", "Use only letters a-z, digits and . _ + - @"],
    ["o'brien@example.com", "Use only letters a-z, digits and . _ + - @"],
    ["owner", "Use exactly one @"],
    ["a@b@example.com", "Use exactly one @"],
    ["@example.com", "Add the name before the @"],
    ["a@localhost", "Add a domain such as example.com after the @"],
    ["a@.example.com", "Add a domain such as example.com after the @"],
    ["a@example..com", "Add a domain such as example.com after the @"],
    ["a@example.com.", "Add a domain such as example.com after the @"],
  ])("rejects %j", (email, message) => {
    expect(emailError(email)).toBe(message);
  });

  it("normalises like the server", () => {
    expect(normalizeEmail("  Owner@Example.COM\t")).toBe("owner@example.com");
  });

  it("counts code points, not UTF-16 units", () => {
    expect(codePoints("😀".repeat(12))).toBe(12);
    const ok = {
      displayName: "",
      email: "a@example.com",
      password: "😀".repeat(12),
      confirm: "😀".repeat(12),
    };
    expect(validateRegisterForm(ok)).toEqual({});
  });

  it("checks length, the email and the confirmation", () => {
    const base = {
      displayName: "",
      email: "a@example.com",
      password: "x".repeat(12),
      confirm: "x".repeat(12),
    };
    expect(validateRegisterForm(base)).toEqual({});
    expect(validateRegisterForm({ ...base, password: "x".repeat(11), confirm: "" })).toEqual({
      password: "Use at least 12 characters",
    });
    expect(
      validateRegisterForm({ ...base, password: "x".repeat(129), confirm: "x".repeat(129) }),
    ).toEqual({ password: "Use at most 128 characters" });
    expect(
      validateRegisterForm({ ...base, password: "x".repeat(128), confirm: "x".repeat(128) }),
    ).toEqual({});
    expect(
      validateRegisterForm({
        displayName: "",
        email: "Long.Address@Example.com",
        password: "long.address@example.COM",
        confirm: "long.address@example.COM",
      }),
    ).toEqual({ password: "Do not use your email as the password" });
    expect(validateRegisterForm({ ...base, confirm: "y".repeat(12) })).toEqual({
      confirm: "The passwords do not match",
    });
    expect(validateRegisterForm({ displayName: "", email: "", password: "", confirm: "" })).toEqual(
      {
        email: "Enter your email address",
        password: "Use at least 12 characters",
      },
    );
  });

  it("checks the optional display name like the server", () => {
    expect(displayNameError("")).toBeUndefined();
    expect(displayNameError("  Ann Lee  ")).toBeUndefined();
    expect(displayNameError("é".repeat(80))).toBeUndefined();
    expect(displayNameError("é".repeat(81))).toBe("Use at most 80 characters");
    expect(displayNameError("Ann\u202eeL")).toBe("Remove control or invisible characters");
    expect(displayNameError("Ann\u200dLee")).toBe("Remove control or invisible characters");
    expect(displayNameError("Ann\tLee")).toBe("Remove control or invisible characters");
  });
});
