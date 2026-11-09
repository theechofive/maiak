/**
 * How a member is named in the UI. The API shows logins (email addresses) to owners and admins
 * only (L-278); everyone else gets the display name, or for a user without one a stable label
 * from the public user id. The id is a UUIDv7 whose first digits are a timestamp shared by users
 * created around the same time, so the label uses the last four (random) hex digits.
 */
export function memberLabel(m: {
  userId: string;
  login: string | null;
  displayName: string;
}): string {
  return m.login ?? (m.displayName.trim() || `Member ${m.userId.replace(/-/g, "").slice(-4)}`);
}
