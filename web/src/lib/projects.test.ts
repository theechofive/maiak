import type { Project } from "../api/endpoints";
import {
  ALL_PROJECTS,
  allProjectsLabel,
  canEditIn,
  channelScopeLabel,
  defaultProjectFor,
  editableProjects,
  projectStorageKey,
  readProjectChoice,
  resolveProjectChoice,
  saveProjectChoice,
} from "./projects";
import { channelsForProject } from "./monitorForm";

const project = (over: Partial<Project>): Project => ({
  id: "p",
  name: "P",
  slug: "p",
  isDefault: false,
  allMembers: false,
  role: "member",
  monitorCount: 0,
  channelCount: 0,
  createdAt: "2026-11-01T00:00:00Z",
  updatedAt: "2026-11-01T00:00:00Z",
  ...over,
});

const def = project({ id: "d", name: "Default", isDefault: true, allMembers: true });
const web = project({ id: "w", name: "Web" });
const ro = project({ id: "r", name: "Read only", role: "viewer" });

describe("project choice", () => {
  afterEach(() => localStorage.clear());

  it("is remembered per user and organization", () => {
    const a = projectStorageKey("u1", "t1");
    const b = projectStorageKey("u2", "t1");
    expect(readProjectChoice(a)).toBe(ALL_PROJECTS);
    saveProjectChoice(a, "w");
    expect(readProjectChoice(a)).toBe("w");
    expect(readProjectChoice(b)).toBe(ALL_PROJECTS);
    saveProjectChoice(a, ALL_PROJECTS);
    expect(localStorage.getItem(a)).toBeNull();
  });

  it("falls back to all projects when the remembered one is gone", () => {
    expect(resolveProjectChoice("w", [def, web])).toBe("w");
    expect(resolveProjectChoice("gone", [def, web])).toBe(ALL_PROJECTS);
    // While the list loads, the choice stands.
    expect(resolveProjectChoice("w", undefined)).toBe("w");
  });

  it("labels the whole scope by role", () => {
    expect(allProjectsLabel("admin")).toBe("All projects");
    expect(allProjectsLabel("member")).toBe("All my projects");
  });
});

describe("project rights", () => {
  it("lets members change monitors only where their role is member or more", () => {
    expect(canEditIn(web)).toBe(true);
    expect(canEditIn(ro)).toBe(false);
    expect(editableProjects([def, web, ro]).map((p) => p.id)).toEqual(["d", "w"]);
  });

  it("starts a new monitor in the shell's project when allowed, else the Default one", () => {
    expect(defaultProjectFor("w", [def, web, ro])).toBe("w");
    expect(defaultProjectFor("r", [def, web, ro])).toBe("d");
    expect(defaultProjectFor(ALL_PROJECTS, [def, web])).toBe("d");
    expect(defaultProjectFor(ALL_PROJECTS, undefined)).toBe("");
  });

  it("names a channel's scope", () => {
    expect(channelScopeLabel(null, [def])).toBe("Organization-wide");
    expect(channelScopeLabel("w", [def, web])).toBe("Web");
    expect(channelScopeLabel("x", [def])).toBe("Another project");
  });

  it("keeps only channels the new project may use when a monitor moves", () => {
    const ch = (id: string, projectId: string | null) => ({
      id,
      projectId,
      name: id,
      type: "slack_webhook" as const,
      events: ["down" as const],
      mention: [],
      reminderMinutes: 0,
      status: "active" as const,
      createdAt: "",
      updatedAt: "",
    });
    const known = [ch("a", "d"), ch("b", "w"), ch("o", null)];
    expect(channelsForProject(["a", "b", "o", "unknown"], "w", known)).toEqual([
      "b",
      "o",
      "unknown",
    ]);
  });
});
