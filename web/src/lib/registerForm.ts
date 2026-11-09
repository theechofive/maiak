/** Client-side mirror of the server's registration policy (P-02, ADR-0009 amendment 2026-10-21). */

export interface RegisterFormValues {
  /** Optional; what members and viewers see instead of the email (L-278). */
  displayName: string;
  email: string;
  password: string;
  confirm: string;
}

export type RegisterField = keyof RegisterFormValues;
export type RegisterFormErrors = Partial<Record<RegisterField, string>>;

export const EMAIL_MIN = 3;
export const EMAIL_MAX = 254;
export const PASSWORD_MIN = 12;
export const PASSWORD_MAX = 128;
export const DISPLAY_NAME_MAX = 80;

/** Control and invisible formatting characters, refused in names like on the server. */
const HIDDEN_CHARS = /[\p{Cc}\p{Cf}]/u;

/** The server's display name rule (optional); returns a message or undefined. */
export function displayNameError(raw: string): string | undefined {
  const name = raw.trim();
  if (codePoints(name) > DISPLAY_NAME_MAX) return `Use at most ${DISPLAY_NAME_MAX} characters`;
  if (HIDDEN_CHARS.test(name)) return "Remove control or invisible characters";
  return undefined;
}

/** The login alphabet: every registered email must be usable as a login. */
const EMAIL_CHARS = /^[A-Za-z0-9._+@-]+$/;

/** Trimmed and lowercased, like the server stores it. */
export function normalizeEmail(email: string): string {
  return email.trim().toLowerCase();
}

/** The server's email rule; returns a message or undefined. */
export function emailError(raw: string): string | undefined {
  const email = raw.trim();
  if (email === "") return "Enter your email address";
  if (email.length < EMAIL_MIN || email.length > EMAIL_MAX) {
    return `Use ${EMAIL_MIN}-${EMAIL_MAX} characters`;
  }
  if (!EMAIL_CHARS.test(email)) return "Use only letters a-z, digits and . _ + - @";
  const parts = email.split("@");
  if (parts.length !== 2) return "Use exactly one @";
  const [local, domain] = parts as [string, string];
  if (local === "") return "Add the name before the @";
  if (
    !domain.includes(".") ||
    domain.startsWith(".") ||
    domain.endsWith(".") ||
    domain.includes("..")
  ) {
    return "Add a domain such as example.com after the @";
  }
  return undefined;
}

/** Length in Unicode code points, as the server counts it. */
export function codePoints(s: string): number {
  return [...s].length;
}

export function validateRegisterForm(values: RegisterFormValues): RegisterFormErrors {
  const errors: RegisterFormErrors = {};
  const displayName = displayNameError(values.displayName);
  if (displayName) errors.displayName = displayName;
  const email = emailError(values.email);
  if (email) errors.email = email;
  const length = codePoints(values.password);
  if (length < PASSWORD_MIN) {
    errors.password = `Use at least ${PASSWORD_MIN} characters`;
  } else if (length > PASSWORD_MAX) {
    errors.password = `Use at most ${PASSWORD_MAX} characters`;
  } else if (!email && values.password.toLowerCase() === normalizeEmail(values.email)) {
    errors.password = "Do not use your email as the password";
  }
  if (!errors.password && values.confirm !== values.password) {
    errors.confirm = "The passwords do not match";
  }
  return errors;
}
