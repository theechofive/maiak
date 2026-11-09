/**
 * How a member is named in the UI. The API shows logins (email addresses) to owners and admins
 * only (L-278); everyone else gets the display name.
 */
export function memberLabel(m: { login: string | null; displayName: string }): string {
  return m.login ?? (m.displayName.trim() || "A member");
}
