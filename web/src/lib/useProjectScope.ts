import { useQuery } from "@tanstack/react-query";
import { listProjects, queryKeys } from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import {
  ALL_PROJECTS,
  projectStorageKey,
  resolveProjectChoice,
  useProjectChoice,
} from "./projects";

/** The projects of the user and the one the shell is filtered to (undefined: all of them). */
export function useProjectScope() {
  const session = useAuth().session;
  const key = session ? projectStorageKey(session.user.id, session.tenant.id) : null;
  const [saved, setChoice] = useProjectChoice(key);
  const projects = useQuery({
    queryKey: queryKeys.projects,
    queryFn: listProjects,
    enabled: session !== null && session !== undefined,
    staleTime: 30_000,
  });
  const choice = resolveProjectChoice(saved, projects.data?.items);
  return {
    projects,
    choice,
    setChoice,
    projectId: choice === ALL_PROJECTS ? undefined : choice,
  };
}
