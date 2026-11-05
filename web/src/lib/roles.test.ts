import type { SessionInfo } from "../api/endpoints";
import { abilities, assignableRoles, canManageMember, roleAtLeast } from "./roles";

const as = (role: SessionInfo["role"]) => ({ role }) as SessionInfo;

describe("roles", () => {
  it("orders the roles", () => {
    expect(roleAtLeast("owner", "admin")).toBe(true);
    expect(roleAtLeast("member", "member")).toBe(true);
    expect(roleAtLeast("viewer", "member")).toBe(false);
    expect(roleAtLeast(undefined, "viewer")).toBe(false);
  });

  it("derives what a session may do", () => {
    expect(abilities(as("viewer"))).toMatchObject({
      editMonitors: false,
      manageChannels: false,
      manageMembers: false,
      deleteOrganization: false,
    });
    expect(abilities(as("member"))).toMatchObject({ editMonitors: true, manageChannels: false });
    expect(abilities(as("admin"))).toMatchObject({
      manageChannels: true,
      manageMembers: true,
      deleteOrganization: false,
    });
    expect(abilities(as("owner")).deleteOrganization).toBe(true);
    expect(abilities(null).editMonitors).toBe(false);
  });

  it("never offers a role above the actor's own", () => {
    expect(assignableRoles("owner")).toEqual(["owner", "admin", "member", "viewer"]);
    expect(assignableRoles("admin")).toEqual(["admin", "member", "viewer"]);
    expect(assignableRoles("member")).toEqual([]);
  });

  it("keeps admins away from owners", () => {
    expect(canManageMember("admin", "owner")).toBe(false);
    expect(canManageMember("admin", "admin")).toBe(true);
    expect(canManageMember("owner", "owner")).toBe(true);
    expect(canManageMember("member", "viewer")).toBe(false);
  });
});
