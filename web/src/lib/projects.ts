import { useCallback, useSyncExternalStore } from "react";
import type { MembershipRole, Project } from "../api/endpoints";
import { roleAtLeast } from "./roles";

/**
 * The project the shell is filtered to (ADR-0102, T-02): one project or "all" (every project the
 * user sees). Remembered per user and organization in this browser; the server still decides
 * what each role may see.
 */
export const ALL_PROJECTS = "all";

export function projectStorageKey(userId: string, tenantId: string): string {
  return `maiak-project:${userId}:${tenantId}`;
}

const listeners = new Set<() => void>();
// The choice per key when storage is blocked, so the controls agree until a reload.
const fallback = new Map<string, string>();

export function readProjectChoice(key: string): string {
  try {
    return localStorage.getItem(key) ?? ALL_PROJECTS;
  } catch {
    return fallback.get(key) ?? ALL_PROJECTS;
  }
}

export function saveProjectChoice(key: string, choice: string): void {
  fallback.set(key, choice);
  try {
    if (choice === ALL_PROJECTS) localStorage.removeItem(key);
    else localStorage.setItem(key, choice);
  } catch {
    // Storage blocked: the choice lasts until the page is reloaded.
  }
  listeners.forEach((notify) => notify());
}

function subscribe(notify: () => void) {
  listeners.add(notify);
  return () => listeners.delete(notify);
}

/**
 * A remembered choice that is no longer valid (the project was deleted, or the user lost access)
 * falls back to all projects.
 */
export function resolveProjectChoice(choice: string, projects: Project[] | undefined): string {
  if (choice === ALL_PROJECTS || projects === undefined) return choice;
  return projects.some((p) => p.id === choice) ? choice : ALL_PROJECTS;
}

export function useProjectChoice(key: string | null): [string, (choice: string) => void] {
  const choice = useSyncExternalStore(subscribe, () =>
    key === null ? ALL_PROJECTS : readProjectChoice(key),
  );
  const set = useCallback(
    (c: string) => {
      if (key !== null) saveProjectChoice(key, c);
    },
    [key],
  );
  return [choice, set];
}

/** Whether the role may create and change monitors in a project (its role there, ADR-0102). */
export function canEditIn(project: Pick<Project, "role"> | undefined): boolean {
  return roleAtLeast(project?.role, "member");
}

/** The projects a monitor may be created in or moved to by this user. */
export function editableProjects(projects: Project[] | undefined): Project[] {
  return (projects ?? []).filter((p) => canEditIn(p));
}

/** The label of the "every project" entry: admins see the whole organization. */
export function allProjectsLabel(role: MembershipRole | undefined): string {
  return roleAtLeast(role, "admin") ? "All projects" : "All my projects";
}

/** The default project of a new monitor: the shell's project when the user may edit there. */
export function defaultProjectFor(choice: string, projects: Project[] | undefined): string {
  const editable = editableProjects(projects);
  if (editable.some((p) => p.id === choice)) return choice;
  return (editable.find((p) => p.isDefault) ?? editable[0])?.id ?? "";
}

/** "Organization-wide" for a channel without a project, the project's name otherwise. */
export function channelScopeLabel(
  projectId: string | null | undefined,
  projects: Project[] | undefined,
): string {
  if (!projectId) return "Organization-wide";
  return projects?.find((p) => p.id === projectId)?.name ?? "Another project";
}
