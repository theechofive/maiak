import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { errorMessage } from "../api/client";
import {
  createInvitation,
  deleteOrganization,
  leaveOrganization,
  listInvitations,
  listMembers,
  queryKeys,
  removeMember,
  revokeInvitation,
  updateMember,
  type Invitation,
  type InvitationRole,
  type Member,
  type MembershipRole,
} from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import { useToast } from "../components/Toasts";
import {
  Alert,
  Badge,
  Button,
  ConfirmDialog,
  CopyField,
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
  type BadgeTone,
} from "../components/ui";
import { formatDate, formatDateTime } from "../lib/format";
import {
  abilities,
  assignableRoles,
  canManageMember,
  ROLE_DESCRIPTIONS,
  ROLE_HINTS,
  ROLE_LABELS,
} from "../lib/roles";

/** Members, roles and invitations of the organization (T-01, ADR-0102). */
export default function Members() {
  const auth = useAuth();
  const can = abilities(auth.session);
  const org = auth.session?.tenant.name ?? "this organization";
  return (
    <section className="flex max-w-[960px] flex-col">
      <h1 className="pt-6 pb-2 text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:pt-10 sm:text-display">
        Members
      </h1>
      <p className="pb-6 text-muted-foreground sm:pb-9">
        People in <strong className="font-medium text-foreground">{org}</strong>. You are{" "}
        {can.role ? `${article(can.role)} ${ROLE_LABELS[can.role].toLowerCase()}` : "a member"}:{" "}
        {can.role ? ROLE_DESCRIPTIONS[can.role].toLowerCase() : ""}
      </p>
      <Section id="members" title="People">
        <MemberList />
      </Section>
      <Section
        id="invitations"
        title="Invitations"
        description="A link joins one person with the role you choose. It works once and for 7 days. No email is sent: hand it over yourself."
      >
        {can.manageMembers ? (
          <Invitations />
        ) : (
          <p className="text-muted-foreground">{ROLE_HINTS.manageMembers}</p>
        )}
      </Section>
      <Section id="leave" title="Leave or delete">
        <LeaveOrDelete />
      </Section>
    </section>
  );
}

function article(role: MembershipRole) {
  return role === "owner" || role === "admin" ? "an" : "a";
}

function Section({
  id,
  title,
  description,
  children,
}: {
  id: string;
  title: string;
  description?: string;
  children: ReactNode;
}) {
  return (
    <section
      aria-labelledby={`${id}-heading`}
      className="grid gap-4 border-t border-border pt-6 pb-7 sm:grid-cols-[220px_minmax(0,1fr)] sm:gap-12 sm:pt-8 sm:pb-10"
    >
      <div className="flex flex-col gap-1.5">
        <h2 id={`${id}-heading`} className="text-lead font-semibold">
          {title}
        </h2>
        {description && <p className="text-[13px] text-muted-foreground">{description}</p>}
      </div>
      <div className="flex min-w-0 flex-col gap-4">{children}</div>
    </section>
  );
}

function MemberList() {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const me = auth.session?.user.id;
  const actor = auth.session?.role;
  const members = useQuery({ queryKey: queryKeys.members, queryFn: listMembers });
  const [removing, setRemoving] = useState<Member | null>(null);

  const change = useMutation({
    mutationFn: ({ member, role }: { member: Member; role: MembershipRole }) =>
      updateMember(member.userId, role),
    onSuccess: (updated) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.members });
      if (updated.userId === me) void queryClient.invalidateQueries({ queryKey: queryKeys.me });
      toast(`${updated.login} is now ${ROLE_LABELS[updated.role].toLowerCase()}`);
    },
    onError: (err) => toast(errorMessage(err), "error"),
  });

  const remove = useMutation({
    mutationFn: (member: Member) => removeMember(member.userId),
    onSuccess: (_, member) => {
      setRemoving(null);
      void queryClient.invalidateQueries({ queryKey: queryKeys.members });
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      toast(`${member.login} was removed`);
    },
    onError: (err) => {
      setRemoving(null);
      toast(errorMessage(err), "error");
    },
  });

  if (members.isPending) {
    return (
      <Loading label="Loading members…">
        <Skeleton className="w-full" />
        <Skeleton className="w-2/3" />
      </Loading>
    );
  }
  if (members.isError) {
    return (
      <ErrorState
        variant="section"
        title="Cannot load the members"
        onRetry={() => void members.refetch()}
      />
    );
  }
  const roles = assignableRoles(actor);
  return (
    <>
      <Table>
        <thead>
          <tr>
            <Th>Login</Th>
            <Th>Role</Th>
            <Th numeric>Joined</Th>
            <Th numeric>
              <span className="sr-only">Actions</span>
            </Th>
          </tr>
        </thead>
        <tbody>
          {members.data.items.map((m) => {
            const manageable = canManageMember(actor, m.role) && !m.isPlatformAdmin;
            const noteId = `member-note-${m.userId}`;
            const note = m.isPlatformAdmin
              ? "The platform administrator; set by the deployment."
              : actor && canManageMember(actor, "viewer") && !canManageMember(actor, m.role)
                ? "Only an owner can change an owner."
                : undefined;
            return (
              <Tr key={m.userId}>
                <Td>
                  <span className="flex flex-wrap items-center gap-2">
                    <span className="font-mono text-[13px] break-all">{m.login}</span>
                    {m.userId === me && <Badge>You</Badge>}
                  </span>
                </Td>
                <Td>
                  {manageable ? (
                    <Select
                      aria-label={`Role of ${m.login}`}
                      className="w-[130px]"
                      value={m.role}
                      disabled={change.isPending}
                      onChange={(e) =>
                        change.mutate({ member: m, role: e.target.value as MembershipRole })
                      }
                    >
                      {roles.map((r) => (
                        <option key={r} value={r}>
                          {ROLE_LABELS[r]}
                        </option>
                      ))}
                    </Select>
                  ) : (
                    <span aria-describedby={note ? noteId : undefined}>{ROLE_LABELS[m.role]}</span>
                  )}
                  {note && (
                    <span id={noteId} className="mt-1 block text-caption text-muted-foreground">
                      {note}
                    </span>
                  )}
                </Td>
                <Td numeric>{formatDate(m.joinedAt)}</Td>
                <Td numeric>
                  {manageable && m.userId !== me && (
                    <Button variant="ghost-destructive" size="small" onClick={() => setRemoving(m)}>
                      Remove
                    </Button>
                  )}
                </Td>
              </Tr>
            );
          })}
        </tbody>
      </Table>
      {removing && (
        <ConfirmDialog
          title={`Remove ${removing.login}?`}
          confirmLabel="Remove member"
          busy={remove.isPending}
          onCancel={() => setRemoving(null)}
          onConfirm={() => remove.mutate(removing)}
        >
          <p>
            They lose access to this organization at once and are signed out of it. Their account
            and other organizations stay. Invite them again to bring them back.
          </p>
        </ConfirmDialog>
      )}
    </>
  );
}

const STATUS_TONES: Record<Invitation["status"], BadgeTone> = {
  open: "neutral",
  accepted: "success",
  revoked: "deleted",
  expired: "deleted",
};

/** The link of a new invitation; kept in component state only, never stored (R-213). */
interface CreatedLink {
  url: string;
  role: InvitationRole;
}

function absoluteLink(url: string) {
  // The api builds it from MAIAK_PUBLIC_URL; without one it is a path on this origin.
  return url.startsWith("/") ? `${globalThis.location.origin}${url}` : url;
}

function Invitations() {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const invitations = useQuery({ queryKey: queryKeys.invitations, queryFn: listInvitations });
  const [dialog, setDialog] = useState(false);
  const [role, setRole] = useState<InvitationRole>("member");
  const [label, setLabel] = useState("");
  const [created, setCreated] = useState<CreatedLink | null>(null);
  const [error, setError] = useState<string | undefined>();
  const [revoking, setRevoking] = useState<Invitation | null>(null);
  const roles = assignableRoles(auth.session?.role).filter(
    (r): r is InvitationRole => r !== "owner",
  );

  const create = useMutation({
    mutationFn: () => createInvitation(role, label.trim()),
    // The answer holds the secret link: it must not stay in the query cache.
    gcTime: 0,
    onSuccess: (res) => {
      setDialog(false);
      setCreated({ url: absoluteLink(res.inviteUrl), role: res.invitation.role });
      setLabel("");
      create.reset();
      void queryClient.invalidateQueries({ queryKey: queryKeys.invitations });
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
    },
    onError: (err) => setError(errorMessage(err)),
  });

  const revoke = useMutation({
    mutationFn: (inv: Invitation) => revokeInvitation(inv.id),
    onSuccess: () => {
      setRevoking(null);
      void queryClient.invalidateQueries({ queryKey: queryKeys.invitations });
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      toast("Invitation revoked");
    },
    onError: (err) => {
      setRevoking(null);
      toast(errorMessage(err), "error");
    },
  });

  return (
    <>
      {created ? (
        <section
          aria-labelledby="invite-link-heading"
          className="flex flex-col gap-4 border border-border p-5"
        >
          <h3 id="invite-link-heading" className="text-lead font-semibold">
            Invitation link ({ROLE_LABELS[created.role].toLowerCase()})
          </h3>
          <CopyField label="Invitation link" value={created.url} copyLabel="Copy link" />
          <p className="text-muted-foreground">
            Anyone with this link can join as {ROLE_LABELS[created.role].toLowerCase()} until it is
            used, revoked or 7 days pass. Send it over a channel you trust, not a public chat. Maiak
            keeps only a hash of it.
          </p>
          <div>
            <Button variant="secondary" onClick={() => setCreated(null)}>
              I sent it, hide the link
            </Button>
          </div>
        </section>
      ) : (
        <div>
          <Button
            onClick={() => {
              setError(undefined);
              setDialog(true);
            }}
          >
            Invite someone
          </Button>
        </div>
      )}

      {invitations.isPending && (
        <Loading label="Loading invitations…">
          <Skeleton className="w-full" />
        </Loading>
      )}
      {invitations.isError && (
        <ErrorState
          variant="section"
          title="Cannot load the invitations"
          onRetry={() => void invitations.refetch()}
        />
      )}
      {invitations.data && invitations.data.items.length === 0 && (
        <EmptyNote>No invitations yet.</EmptyNote>
      )}
      {invitations.data && invitations.data.items.length > 0 && (
        <Table>
          <thead>
            <tr>
              <Th>Invitation</Th>
              <Th>Role</Th>
              <Th>Status</Th>
              <Th numeric>Expires</Th>
              <Th numeric>
                <span className="sr-only">Actions</span>
              </Th>
            </tr>
          </thead>
          <tbody>
            {invitations.data.items.map((inv) => (
              <Tr key={inv.id}>
                <Td>
                  <span className="block">{inv.label || "No note"}</span>
                  <span className="block text-caption text-muted-foreground">
                    by {inv.createdBy}
                    {inv.acceptedBy ? ` · joined: ${inv.acceptedBy}` : ""}
                  </span>
                </Td>
                <Td>{ROLE_LABELS[inv.role]}</Td>
                <Td>
                  <Badge tone={STATUS_TONES[inv.status]}>{inv.status}</Badge>
                </Td>
                <Td numeric>
                  {inv.status === "open"
                    ? formatDateTime(inv.expiresAt)
                    : formatDate(inv.expiresAt)}
                </Td>
                <Td numeric>
                  {inv.status === "open" && (
                    <Button
                      variant="ghost-destructive"
                      size="small"
                      onClick={() => setRevoking(inv)}
                    >
                      Revoke
                    </Button>
                  )}
                </Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      )}

      {dialog && (
        <ConfirmDialog
          title="Invite someone"
          confirmLabel="Create link"
          confirmVariant="primary"
          busy={create.isPending}
          onCancel={() => setDialog(false)}
          onConfirm={() => {
            setError(undefined);
            create.mutate();
          }}
        >
          <div className="flex flex-col gap-4">
            {error && <Alert>{error}</Alert>}
            <Field label="Role" hint={ROLE_DESCRIPTIONS[role]}>
              {(c) => (
                <Select
                  {...c}
                  value={role}
                  onChange={(e) => setRole(e.target.value as InvitationRole)}
                >
                  {roles.map((r) => (
                    <option key={r} value={r}>
                      {ROLE_LABELS[r]}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
            <Field label="Note (optional)" hint="For you only, for example the person's name.">
              {(c) => (
                <Input
                  {...c}
                  maxLength={80}
                  value={label}
                  onChange={(e) => setLabel(e.target.value)}
                />
              )}
            </Field>
          </div>
        </ConfirmDialog>
      )}
      {revoking && (
        <ConfirmDialog
          title="Revoke this invitation?"
          confirmLabel="Revoke invitation"
          busy={revoke.isPending}
          onCancel={() => setRevoking(null)}
          onConfirm={() => revoke.mutate(revoking)}
        >
          <p>The link stops working at once and its place in your plan is freed.</p>
        </ConfirmDialog>
      )}
    </>
  );
}

function LeaveOrDelete() {
  const auth = useAuth();
  const navigate = useNavigate();
  const toast = useToast();
  const can = abilities(auth.session);
  const org = auth.session?.tenant;
  const [confirm, setConfirm] = useState<"leave" | "delete" | null>(null);

  const leave = useMutation({
    mutationFn: leaveOrganization,
    onSuccess: () => {
      auth.endSession();
      navigate("/login", { replace: true });
    },
    onError: (err) => {
      setConfirm(null);
      toast(errorMessage(err), "error");
    },
  });
  const remove = useMutation({
    mutationFn: deleteOrganization,
    onSuccess: () => {
      auth.endSession();
      navigate("/login", { replace: true });
    },
    onError: (err) => {
      setConfirm(null);
      toast(errorMessage(err), "error");
    },
  });
  if (!org) return null;
  const isDefault = org.slug === "default";

  return (
    <>
      <div className="flex flex-col gap-2">
        <p className="text-muted-foreground">
          Leaving ends your access to {org.name}; your account and other organizations stay. An
          organization always keeps an owner: make someone else an owner before you leave as the
          last one.
        </p>
        <div>
          <Button variant="secondary" onClick={() => setConfirm("leave")}>
            Leave organization
          </Button>
        </div>
      </div>
      {can.deleteOrganization && !isDefault && (
        <div className="flex flex-col gap-2 border-t border-border-faint pt-4">
          <p className="text-muted-foreground">
            Deleting stops every monitor and alert of {org.name} and signs every member out of it.
            The data is kept but nobody can use it any more.
          </p>
          <div>
            <Button variant="ghost-destructive" onClick={() => setConfirm("delete")}>
              Delete organization
            </Button>
          </div>
        </div>
      )}
      {can.deleteOrganization && isDefault && (
        <p className="text-caption text-muted-foreground">
          This is the platform&apos;s own organization: it cannot be deleted.
        </p>
      )}
      {confirm === "leave" && (
        <ConfirmDialog
          title={`Leave ${org.name}?`}
          confirmLabel="Leave"
          busy={leave.isPending}
          onCancel={() => setConfirm(null)}
          onConfirm={() => leave.mutate()}
        >
          <p>You are signed out. To come back, someone has to invite you again.</p>
        </ConfirmDialog>
      )}
      {confirm === "delete" && (
        <ConfirmDialog
          title={`Delete ${org.name}?`}
          confirmLabel="Delete organization"
          busy={remove.isPending}
          onCancel={() => setConfirm(null)}
          onConfirm={() => remove.mutate()}
        >
          <p>
            Monitors stop, alerts are no longer sent and every member loses access. The app cannot
            undo this; the platform administrator can restore it.
          </p>
        </ConfirmDialog>
      )}
    </>
  );
}
