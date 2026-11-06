import { useAuth } from "../auth/AuthProvider";
import { ALL_PROJECTS, allProjectsLabel } from "../lib/projects";
import { useProjectScope } from "../lib/useProjectScope";
import { cx, Select } from "./ui";

/**
 * The project filter of the shell (ADR-0102, T-02), remembered per user and organization in this
 * browser. Hidden while the organization has only its Default project.
 */
export default function ProjectSwitcher({ className }: { className?: string }) {
  const role = useAuth().session?.role;
  const { projects, choice, setChoice } = useProjectScope();
  const items = projects.data?.items ?? [];
  if (items.length < 2) return null;
  return (
    <Select
      aria-label="Project filter"
      className={cx("max-w-[200px]", className)}
      value={choice}
      onChange={(e) => setChoice(e.target.value)}
    >
      <option value={ALL_PROJECTS}>{allProjectsLabel(role)}</option>
      {items.map((p) => (
        <option key={p.id} value={p.id}>
          {p.name}
        </option>
      ))}
    </Select>
  );
}
