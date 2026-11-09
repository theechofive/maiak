import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiError, errorMessage, errorMeta, planLimitMessage } from "../api/client";
import {
  createProject,
  deleteProject,
  listMembers,
  listProjectMembers,
  putProjectMember,
  queryKeys,
  removeProjectMember,
  renameProject,
  type Project,
  type ProjectMember,
} from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import { useToast } from "../components/Toasts";
import {
  Alert,
  Badge,
  Button,
  ConfirmDialog,
  EmptyNote,
  ErrorState,
  Field,
  Input,
  Loading,
  Select,
  Skeleton,
  Table,
  Td,
  Th,
  Tr,
} from "../components/ui";
import { countOf } from "../lib/usage";
import { abilities, ROLE_LABELS } from "../lib/roles";
import { useProjectScope } from "../lib/useProjectScope";
import { memberLabel } from "../lib/members";

const NAME_MAX = 60;

/** Projects of the organization and who belongs to them (T-02, ADR-0102). */
export default function Projects() {
  const can = abilities(useAuth().session);
  const manage = can.manageMembers;
  const { projects } = useProjectScope();
  const [open, setOpen] = useState<string | null>(null);
  const items = projects.data?.items ?? [];
  const shown = items.find((p) => p.id === open);

  return (
    <section className="flex max-w-[960px] flex-col gap-6">
      <div className="flex flex-col gap-2 pt-6 sm:pt-10">
        <h1 className="text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:text-display">
          Projects
        </h1>
        <p className="text-muted-foreground">
          Projects split the monitors and notification channels of the organization. Owners and
          admins see every project; members and viewers see the projects they belong to. Everyone
          belongs to the Default project.
        </p>
      </div>

      {manage && <NewProject />}

      {projects.isPending ? (
        <Loading label="Loading projects…">
          <Skeleton className="w-2/3" />
          <Skeleton className="w-1/2" />
        </Loading>
      ) : projects.isError ? (
        <ErrorState
          title="Cannot load projects"
          detail={errorMessage(projects.error)}
          meta={errorMeta(projects.error)}
          onRetry={() => void projects.refetch()}
        />
      ) : items.length === 0 ? (
        <EmptyNote>No projects to show.</EmptyNote>
      ) : (
        <ProjectTable
          items={items}
          manage={manage}
          open={open}
          onOpen={(id) => setOpen(open === id ? null : id)}
        />
      )}

      {shown && <ProjectMembers key={shown.id} project={shown} manage={manage} />}
    </section>
  );
}

function NewProject() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [name, setName] = useState("");
  const [error, setError] = useState<string | undefined>();
  const create = useMutation({
    mutationFn: (n: string) => createProject(n),
    onSuccess: (p) => {
      setName("");
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects });
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      toast(`Project ${p.name} created`);
    },
    onError: (err) => setError(planLimitMessage(err) ?? errorMessage(err)),
  });

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) {
      setError("Enter a name.");
      return;
    }
    if ([...trimmed].length > NAME_MAX) {
      setError(`Use ${NAME_MAX} characters or fewer.`);
      return;
    }
    setError(undefined);
    create.mutate(trimmed);
  }

  return (
    <form
      onSubmit={onSubmit}
      noValidate
      aria-label="Create a project"
      className="flex flex-col gap-2 sm:flex-row sm:items-end"
    >
      <Field label="New project" error={error} className="sm:w-80">
        {(c) => (
          <Input
            {...c}
            name="projectName"
            autoComplete="off"
            maxLength={NAME_MAX * 2}
            placeholder="Payments"
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              setError(undefined);
            }}
          />
        )}
      </Field>
      <Button type="submit" busy={create.isPending} className={error ? "sm:mb-7" : undefined}>
        Create project
      </Button>
    </form>
  );
}

function ProjectTable({
  items,
  manage,
  open,
  onOpen,
}: {
  items: Project[];
  manage: boolean;
  open: string | null;
  onOpen: (id: string) => void;
}) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [renaming, setRenaming] = useState<Project | null>(null);
  const [deleting, setDeleting] = useState<Project | null>(null);

  const remove = useMutation({
    mutationFn: (p: Project) => deleteProject(p.id),
    onSuccess: (_, p) => {
      setDeleting(null);
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects });
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      toast(`Project ${p.name} deleted`);
    },
    onError: (err) => {
      setDeleting(null);
      toast(`Delete failed. ${errorMessage(err)}`, "error");
    },
  });

  return (
    <>
      <Table>
        <thead>
          <tr>
            <Th>Name</Th>
            <Th>Monitors</Th>
            <Th className="max-sm:hidden">Channels</Th>
            <Th className="max-sm:hidden">Your role</Th>
            <Th>
              <span className="sr-only">Actions</span>
            </Th>
          </tr>
        </thead>
        <tbody>
          {items.map((p) => (
            <Tr key={p.id}>
              <Td>
                {renaming?.id === p.id ? (
                  <RenameForm project={p} onDone={() => setRenaming(null)} />
                ) : (
                  <span className="flex flex-wrap items-center gap-2">
                    <span className="font-medium break-words">{p.name}</span>
                    {p.isDefault && <Badge>Default</Badge>}
                  </span>
                )}
              </Td>
              <Td className="font-mono text-caption">{p.monitorCount}</Td>
              <Td className="font-mono text-caption max-sm:hidden">{p.channelCount}</Td>
              <Td className="text-caption max-sm:hidden">{ROLE_LABELS[p.role]}</Td>
              <Td>
                <div className="flex justify-end gap-1">
                  <Button
                    variant="ghost"
                    size="small"
                    aria-expanded={open === p.id}
                    aria-label={`People in ${p.name}`}
                    onClick={() => onOpen(p.id)}
                  >
                    People
                  </Button>
                  {manage && (
                    <>
                      <Button
                        variant="ghost"
                        size="small"
                        aria-label={`Rename ${p.name}`}
                        onClick={() => setRenaming(p)}
                      >
                        Rename
                      </Button>
                      <Button
                        variant="ghost-destructive"
                        size="small"
                        aria-label={`Delete ${p.name}`}
                        disabled={p.isDefault}
                        onClick={() => setDeleting(p)}
                      >
                        Delete
                      </Button>
                    </>
                  )}
                </div>
              </Td>
            </Tr>
          ))}
        </tbody>
      </Table>
      {deleting &&
        (deleting.monitorCount > 0 || deleting.channelCount > 0 ? (
          <ConfirmDialog
            title={`${deleting.name} is not empty`}
            confirmLabel="OK"
            confirmVariant="secondary"
            cancelLabel="Close"
            onConfirm={() => setDeleting(null)}
            onCancel={() => setDeleting(null)}
          >
            It still has {countOf(deleting.monitorCount, "monitor", "monitors")} and{" "}
            {countOf(deleting.channelCount, "channel", "channels")}. Move them to another project or
            delete them first.
          </ConfirmDialog>
        ) : (
          <ConfirmDialog
            title={`Delete ${deleting.name}?`}
            confirmLabel="Delete project"
            busy={remove.isPending}
            onConfirm={() => remove.mutate(deleting)}
            onCancel={() => setDeleting(null)}
          >
            The project and its list of people are removed. It has no monitors or channels.
          </ConfirmDialog>
        ))}
    </>
  );
}

function RenameForm({ project, onDone }: { project: Project; onDone: () => void }) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [name, setName] = useState(project.name);
  const [error, setError] = useState<string | undefined>();
  const save = useMutation({
    mutationFn: (n: string) => renameProject(project.id, n),
    onSuccess: (p) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects });
      toast(`Renamed to ${p.name}`);
      onDone();
    },
    onError: (err) => setError(errorMessage(err)),
  });
  return (
    <form
      noValidate
      aria-label={`Rename ${project.name}`}
      className="flex flex-wrap items-start gap-2"
      onSubmit={(e) => {
        e.preventDefault();
        const trimmed = name.trim();
        if (!trimmed || [...trimmed].length > NAME_MAX) {
          setError(`Use 1 to ${NAME_MAX} characters.`);
          return;
        }
        save.mutate(trimmed);
      }}
    >
      <Field label="Project name" error={error} className="min-w-40 flex-1">
        {(c) => (
          <Input
            {...c}
            // eslint-disable-next-line jsx-a11y/no-autofocus -- the user just asked to rename this row
            autoFocus
            value={name}
            maxLength={NAME_MAX * 2}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") onDone();
            }}
          />
        )}
      </Field>
      <span className="flex gap-1 pt-6">
        <Button type="submit" size="small" busy={save.isPending}>
          Save
        </Button>
        <Button variant="ghost" size="small" onClick={onDone}>
          Cancel
        </Button>
      </span>
    </form>
  );
}

/** Where a person's access to a project comes from. */
function accessLabel(m: ProjectMember, project: Project): string {
  if (m.allProjects) return "Every project";
  if (m.projectRole === "viewer" && m.organizationRole !== "viewer") return "Read-only here";
  if (m.explicit) return "Listed";
  return project.allMembers ? "Everyone" : "Listed";
}

function ProjectMembers({ project, manage }: { project: Project; manage: boolean }) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const members = useQuery({
    queryKey: queryKeys.projectMembers(project.id),
    queryFn: () => listProjectMembers(project.id),
  });
  const org = useQuery({ queryKey: queryKeys.members, queryFn: listMembers, enabled: manage });
  const [adding, setAdding] = useState("");

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.projectMembers(project.id) });
    void queryClient.invalidateQueries({ queryKey: queryKeys.projects });
  };
  const put = useMutation({
    mutationFn: ({ userId, readOnly }: { userId: string; readOnly: boolean }) =>
      putProjectMember(project.id, userId, readOnly ? "viewer" : undefined),
    onSuccess: (m) => {
      refresh();
      setAdding("");
      toast(`${memberLabel(m)} in ${project.name}: ${ROLE_LABELS[m.effectiveRole].toLowerCase()}`);
    },
    onError: (err) => toast(errorMessage(err), "error"),
  });
  const remove = useMutation({
    mutationFn: (m: ProjectMember) => removeProjectMember(project.id, m.userId),
    onSuccess: (_, m) => {
      refresh();
      toast(
        project.allMembers
          ? `${memberLabel(m)} keeps the organization role in ${project.name}`
          : `${memberLabel(m)} removed from ${project.name}`,
      );
    },
    onError: (err) => toast(errorMessage(err), "error"),
  });

  const listed = new Set((members.data?.items ?? []).map((m) => m.userId));
  // Owners and admins see every project already; only members and viewers can be added.
  const candidates = (org.data?.items ?? []).filter(
    (m) => !listed.has(m.userId) && (m.role === "member" || m.role === "viewer"),
  );

  return (
    <section
      aria-labelledby="project-people-heading"
      className="flex flex-col gap-4 border-t border-border pt-6"
    >
      <div className="flex flex-col gap-1.5">
        <h2 id="project-people-heading" className="text-lead font-semibold">
          People in {project.name}
        </h2>
        <p className="text-[13px] text-muted-foreground">
          {project.allMembers
            ? "Every member of the organization belongs to this project. A member can be made read-only here."
            : "Owners and admins see every project. Add members and viewers to give them access; a member can be read-only here."}
        </p>
      </div>
      {members.isPending ? (
        <Loading label="Loading people…">
          <Skeleton className="w-1/2" />
        </Loading>
      ) : members.isError ? (
        <ErrorState
          title="Cannot load the people of this project"
          detail={errorMessage(members.error)}
          meta={errorMeta(members.error)}
          onRetry={() => void members.refetch()}
        />
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Login</Th>
              <Th>Organization role</Th>
              <Th>Access</Th>
              <Th>Role here</Th>
              {manage && (
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              )}
            </tr>
          </thead>
          <tbody>
            {members.data.items.map((m) => {
              const adjustable = manage && !m.allProjects && m.organizationRole === "member";
              return (
                <Tr key={m.userId}>
                  <Td>
                    <span
                      className={
                        m.login ? "font-mono text-caption break-all" : "text-caption break-all"
                      }
                    >
                      {memberLabel(m)}
                    </span>
                  </Td>
                  <Td className="text-caption">{ROLE_LABELS[m.organizationRole]}</Td>
                  <Td className="text-caption">{accessLabel(m, project)}</Td>
                  <Td className="text-caption">
                    {adjustable ? (
                      <Select
                        aria-label={`Role of ${memberLabel(m)} in ${project.name}`}
                        value={m.projectRole === "viewer" ? "viewer" : "member"}
                        disabled={put.isPending}
                        onChange={(e) =>
                          put.mutate({ userId: m.userId, readOnly: e.target.value === "viewer" })
                        }
                      >
                        <option value="member">Member</option>
                        <option value="viewer">Viewer (read-only)</option>
                      </Select>
                    ) : (
                      ROLE_LABELS[m.effectiveRole]
                    )}
                  </Td>
                  {manage && (
                    <Td>
                      {m.explicit && (
                        <div className="flex justify-end">
                          <Button
                            variant="ghost-destructive"
                            size="small"
                            aria-label={`Remove ${memberLabel(m)} from ${project.name}`}
                            busy={remove.isPending && remove.variables?.userId === m.userId}
                            onClick={() => remove.mutate(m)}
                          >
                            {project.allMembers ? "Reset" : "Remove"}
                          </Button>
                        </div>
                      )}
                    </Td>
                  )}
                </Tr>
              );
            })}
          </tbody>
        </Table>
      )}
      {manage && !project.allMembers && (
        <form
          aria-label={`Add someone to ${project.name}`}
          className="flex flex-col gap-2 sm:flex-row sm:items-end"
          onSubmit={(e) => {
            e.preventDefault();
            if (adding) put.mutate({ userId: adding, readOnly: false });
          }}
        >
          <Field label="Add someone" className="sm:w-80">
            {(c) => (
              <Select
                {...c}
                value={adding}
                disabled={candidates.length === 0}
                onChange={(e) => setAdding(e.target.value)}
              >
                <option value="">
                  {candidates.length === 0 ? "Everyone is already here" : "Choose a member"}
                </option>
                {candidates.map((m) => (
                  <option key={m.userId} value={m.userId}>
                    {memberLabel(m)} · {ROLE_LABELS[m.role]}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Button type="submit" variant="secondary" disabled={!adding} busy={put.isPending}>
            Add to project
          </Button>
        </form>
      )}
      {members.error instanceof ApiError && members.error.status === 404 && (
        <Alert tone="neutral" title="Not available.">
          This project was deleted or you no longer belong to it.
        </Alert>
      )}
    </section>
  );
}
