import type { MembershipRole, SessionInfo } from "../api/endpoints";

/**
 * Roles of a membership (ADR-0102). The server decides (internal/authz); the UI only hides or
 * disables what the server would refuse, and says why.
 */
const RANK: Record<MembershipRole, number> = { owner: 4, admin: 3, member: 2, viewer: 1 };

export const ROLE_LABELS: Record<MembershipRole, string> = {
  owner: "Owner",
  admin: "Admin",
  member: "Member",
  viewer: "Viewer",
};

export const ROLE_DESCRIPTIONS: Record<MembershipRole, string> = {
  owner: "Everything, including deleting the organization and granting owner.",
  admin: "Members, invitations, notification channels, the status page and every monitor.",
  member: "Creates, edits, pauses and deletes monitors.",
  viewer: "Reads monitors, incidents and channels only.",
};

export function roleAtLeast(role: MembershipRole | undefined, min: MembershipRole): boolean {
  return role !== undefined && RANK[role] >= RANK[min];
}

/** What a session may do in the UI; mirrors the operation table of the API. */
export interface Abilities {
  role: MembershipRole | undefined;
  editMonitors: boolean;
  manageChannels: boolean;
  manageMembers: boolean;
  deleteOrganization: boolean;
  manageStatusPage: boolean;
}

export function abilities(session: SessionInfo | null | undefined): Abilities {
  const role = session?.role;
  return {
    role,
    editMonitors: roleAtLeast(role, "member"),
    manageChannels: roleAtLeast(role, "admin"),
    manageMembers: roleAtLeast(role, "admin"),
    deleteOrganization: role === "owner",
    manageStatusPage: roleAtLeast(role, "admin"),
  };
}

/** The roles an actor may give someone (nobody grants above their own; owner only by an owner). */
export function assignableRoles(actor: MembershipRole | undefined): MembershipRole[] {
  if (!roleAtLeast(actor, "admin")) return [];
  return (["owner", "admin", "member", "viewer"] as const).filter(
    (r) => RANK[r] <= RANK[actor as MembershipRole],
  );
}

/** Whether the actor may change or remove a member with this role (admins never touch owners). */
export function canManageMember(
  actor: MembershipRole | undefined,
  target: MembershipRole,
): boolean {
  return roleAtLeast(actor, "admin") && RANK[target] <= RANK[actor as MembershipRole];
}

/** The friendly reason shown next to a control the role does not allow. */
export const ROLE_HINTS = {
  editMonitors: "Viewers can look but not change monitors. Ask an admin for the member role.",
  manageChannels: "Only admins and owners can change notification channels.",
  manageMembers: "Only admins and owners can invite people or change roles.",
  manageStatusPage: "Only admins and owners can change or publish the status page.",
} as const;
