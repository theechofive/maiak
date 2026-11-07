import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";
import { ApiError, errorMessage } from "../api/client";
import {
  createChannel,
  deleteChannel,
  listChannels,
  queryKeys,
  testChannel,
  updateChannel,
  type Channel,
  type Usage,
} from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import SignInDomains from "../components/SignInDomains";
import { abilities, ROLE_HINTS } from "../lib/roles";
import { useToast } from "../components/Toasts";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  ConfirmDialog,
  DotShape,
  EmptyNote,
  ErrorState,
  Field,
  Input,
  Meter,
  Skeleton,
  Table,
  Td,
  Th,
  Tr,
  TriangleShape,
  WriteOnlyValue,
  Loading,
  Segmented,
  Select,
} from "../components/ui";
import {
  CHANNEL_NAME_MAX,
  channelEvents,
  emptyChannelForm,
  ORGANIZATION_WIDE,
  eventsLabel,
  REMINDER_MAX,
  reminderLabel,
  toChannelCreate,
  toChannelUpdate,
  validateChannelForm,
  valuesFromChannel,
  type ChannelFormErrors,
  type ChannelFormValues,
} from "../lib/channels";
import { formatClock } from "../lib/format";
import { useTheme } from "../lib/theme";
import { channelScopeLabel } from "../lib/projects";
import { useProjectScope } from "../lib/useProjectScope";
import { countOf, useUsage } from "../lib/usage";

export default function Settings() {
  const auth = useAuth();
  const { hash } = useLocation();
  // Links such as /settings#sign-in-domains-heading land on that section: scroll to its heading and
  // move the focus there, on the first render and on every hash change.
  useEffect(() => {
    if (!hash) return;
    const target = document.getElementById(decodeURIComponent(hash.slice(1)));
    if (!target) return;
    target.scrollIntoView?.({ block: "start" });
    target.focus({ preventScroll: true });
  }, [hash]);
  return (
    <section className="flex max-w-[960px] flex-col">
      <h1 className="pt-6 pb-6 text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:pt-10 sm:pb-9 sm:text-display">
        Settings
      </h1>
      <SettingsSection id="plan" title="Plan">
        <PlanUsage />
      </SettingsSection>
      <SettingsSection
        id="channels"
        title="Notification channels"
        description="Slack channels Maiak posts to through Incoming Webhooks. Each channel chooses its events, an optional mention for down alerts and an optional reminder while an incident stays open."
      >
        <NotificationChannels />
      </SettingsSection>
      {auth.session?.role === "owner" && (
        <SettingsSection
          id="sign-in-domains"
          title="Sign-in domains"
          description="People with a Google Workspace account of a claimed domain join this organization on their first sign-in with Google. Only the owner manages this."
        >
          <SignInDomains />
        </SettingsSection>
      )}
      <SettingsSection id="appearance" title="Appearance" description="Saved in this browser only.">
        <Appearance />
      </SettingsSection>
      <SettingsSection id="account" title="Account">
        <div className="flex flex-col gap-1">
          <p>
            Signed in as{" "}
            <strong className="font-mono text-[14px] font-normal break-all">
              {auth.session?.user.login}
            </strong>
            . The password is managed outside the app.{" "}
            <Link to="/profile" className="underline underline-offset-3">
              Sign-in methods
            </Link>{" "}
            lists the providers you sign in with.
          </p>
        </div>
      </SettingsSection>
    </section>
  );
}

function SettingsSection({
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
        <h2 id={`${id}-heading`} tabIndex={-1} className="text-lead font-semibold">
          {title}
        </h2>
        {description && <p className="text-[13px] text-muted-foreground">{description}</p>}
      </div>
      <div className="flex min-w-0 max-w-[560px] flex-col">{children}</div>
    </section>
  );
}

const usageRows: { key: keyof Usage["limits"]; label: string }[] = [
  { key: "monitors", label: "Monitors" },
  { key: "heartbeats", label: "Heartbeat monitors" },
  { key: "channels", label: "Notification channels" },
  { key: "members", label: "Members and open invitations" },
  { key: "projects", label: "Projects" },
];

function PlanUsage() {
  const usage = useUsage();
  if (usage.isPending) {
    return (
      <Loading label="Loading plan and usage…" className="gap-5 pt-1">
        <Skeleton className="h-5 w-28" />
        {usageRows.map((r) => (
          <Skeleton key={r.key} className="w-full" />
        ))}
      </Loading>
    );
  }
  if (usage.isError && !usage.data) {
    return (
      <ErrorState
        variant="section"
        title="Cannot load plan and usage"
        onRetry={() => void usage.refetch()}
      />
    );
  }
  return (
    <>
      <p className="pb-2 text-muted-foreground">
        Your tenant is on the{" "}
        <strong className="text-[22px] font-semibold tracking-[-0.015em] text-foreground">
          {usage.data.plan.name}
        </strong>{" "}
        plan.
      </p>
      <div className="divide-y divide-border-faint border-t border-border-faint">
        {usageRows.map(({ key, label }) => (
          <Meter
            key={key}
            label={label}
            used={usage.data.usage[key]}
            limit={usage.data.limits[key]}
          />
        ))}
      </div>
    </>
  );
}

/** Which form is open: none, a new channel, or the edit form of one channel. */
type Editing = { mode: "none" } | { mode: "create" } | { mode: "edit"; channel: Channel };

type TestResult = { channel: Channel } & (
  { ok: true; at: number } | { ok: false; message: string }
);

function NotificationChannels() {
  const canManage = abilities(useAuth().session).manageChannels;
  const queryClient = useQueryClient();
  const toast = useToast();
  const usage = useUsage();
  const channels = useQuery({ queryKey: queryKeys.channels, queryFn: () => listChannels() });
  const projects = useProjectScope().projects.data?.items;
  const multiProject = (projects?.length ?? 0) > 1;
  const [editing, setEditing] = useState<Editing>({ mode: "none" });
  const [deleting, setDeleting] = useState<Channel | undefined>();
  const [result, setResult] = useState<TestResult | undefined>();

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.channels });
    // Channels count against the plan, and deleting one changes monitors' selections.
    void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
    void queryClient.invalidateQueries({ queryKey: queryKeys.monitors });
    // Project rows count channels.
    void queryClient.invalidateQueries({ queryKey: queryKeys.projects });
  };

  const test = useMutation({
    mutationFn: (c: Channel) => testChannel(c.id),
    onSuccess: (_, c) => setResult({ channel: c, ok: true, at: Date.now() }),
    onError: (err, c) => {
      setResult({ channel: c, ok: false, message: errorMessage(err) });
      // A revoked webhook marks the channel failed.
      void queryClient.invalidateQueries({ queryKey: queryKeys.channels });
    },
  });

  const remove = useMutation({
    mutationFn: (c: Channel) => deleteChannel(c.id),
    onSuccess: (_, c) => {
      setDeleting(undefined);
      if (editing.mode === "edit" && editing.channel.id === c.id) setEditing({ mode: "none" });
      if (result?.channel.id === c.id) setResult(undefined);
      refresh();
      toast(`Channel ${c.name} deleted`);
    },
    onError: (err) => toast(`Delete failed. ${errorMessage(err)}`, "error"),
  });

  if (channels.isPending) {
    return (
      <Loading label="Loading notification channels…" className="gap-4">
        <Skeleton className="w-3/4" />
        <Skeleton className="h-9 w-full" />
      </Loading>
    );
  }
  if (channels.isError) {
    return (
      <ErrorState
        variant="section"
        title="Cannot load notification channels"
        onRetry={() => void channels.refetch()}
      />
    );
  }

  const items = channels.data.items;
  const limit = usage.data?.limits.channels;
  const full = limit !== undefined && (usage.data?.usage.channels ?? items.length) >= limit;

  return (
    <div className="flex flex-col gap-6">
      {items.length === 0 ? (
        <EmptyNote>No notification channels yet. Slack notifications are off.</EmptyNote>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Name</Th>
              {multiProject && <Th>Project</Th>}
              <Th>Events</Th>
              <Th>Mention</Th>
              <Th>Reminder</Th>
              <Th>Status</Th>
              {canManage && (
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              )}
            </tr>
          </thead>
          <tbody>
            {items.map((c) => (
              <Tr key={c.id} tone={c.status === "failed" ? "down" : "default"}>
                <Td>
                  <span className="font-medium break-words">{c.name}</span>
                </Td>
                {multiProject && (
                  <Td className="text-caption">{channelScopeLabel(c.projectId, projects)}</Td>
                )}
                <Td className="text-caption">{eventsLabel(c.events)}</Td>
                <Td className="font-mono text-caption">
                  {c.mention.length > 0 ? c.mention.join(" ") : "None"}
                </Td>
                <Td className="text-caption whitespace-nowrap">
                  {reminderLabel(c.reminderMinutes)}
                </Td>
                <Td>
                  {c.status === "active" ? (
                    <Badge tone="success">Active</Badge>
                  ) : (
                    <Badge tone="danger" icon={<TriangleShape size="small" />}>
                      Failed
                    </Badge>
                  )}
                </Td>
                {canManage && (
                  <Td>
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="small"
                        aria-label={`Edit ${c.name}`}
                        onClick={() => setEditing({ mode: "edit", channel: c })}
                      >
                        Edit
                      </Button>
                      <Button
                        variant="ghost"
                        size="small"
                        aria-label={`Send a test message to ${c.name}`}
                        busy={test.isPending && test.variables?.id === c.id}
                        disabled={test.isPending}
                        onClick={() => test.mutate(c)}
                      >
                        Test
                      </Button>
                      <Button
                        variant="ghost-destructive"
                        size="small"
                        aria-label={`Delete ${c.name}`}
                        onClick={() => setDeleting(c)}
                      >
                        Delete
                      </Button>
                    </div>
                  </Td>
                )}
              </Tr>
            ))}
          </tbody>
        </Table>
      )}

      {items.some((c) => c.status === "failed") && (
        <p className="text-caption text-down">
          A failed channel&apos;s webhook was refused by Slack (probably removed there). Nothing is
          sent to it until you edit it and store a new webhook URL.
        </p>
      )}

      {result?.ok && (
        <span role="status" className="flex items-center gap-2 text-[13px]">
          <DotShape size="large" className="text-success" />
          Test message sent to {result.channel.name} at {formatClock(result.at)}. Look for it in
          your Slack channel.
        </span>
      )}
      {result && !result.ok && (
        <Alert title={`Test message to ${result.channel.name} failed.`}>{result.message}</Alert>
      )}

      {!canManage ? (
        <p className="text-caption text-muted-foreground">{ROLE_HINTS.manageChannels}</p>
      ) : editing.mode === "none" ? (
        <div className="flex flex-col items-start gap-2">
          <Button
            variant={items.length === 0 ? "primary" : "secondary"}
            disabled={full}
            onClick={() => setEditing({ mode: "create" })}
          >
            Add channel
          </Button>
          {limit !== undefined && (
            <span className="text-caption text-muted-foreground">
              {full
                ? `Your plan allows ${countOf(limit, "notification channel", "notification channels")}, and the limit is reached. Delete a channel to add another.`
                : `${countOf(usage.data?.usage.channels ?? items.length, "channel", "channels")} of ${limit} on your plan.`}
            </span>
          )}
        </div>
      ) : (
        <ChannelForm
          key={editing.mode === "edit" ? editing.channel.id : "new"}
          channel={editing.mode === "edit" ? editing.channel : undefined}
          onDone={() => {
            setEditing({ mode: "none" });
            refresh();
          }}
          onCancel={() => setEditing({ mode: "none" })}
        />
      )}

      {deleting && (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          confirmLabel="Delete channel"
          busy={remove.isPending}
          onConfirm={() => remove.mutate(deleting)}
          onCancel={() => setDeleting(undefined)}
        >
          <p>
            Its webhook URL is removed and alerts still queued for it are not sent. Monitors that
            selected only this channel then notify every active channel.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}

/** Creates a channel, or edits one (the stored webhook is kept unless a new URL is typed). */
function ChannelForm({
  channel,
  onDone,
  onCancel,
}: {
  channel?: Channel;
  onDone: () => void;
  onCancel: () => void;
}) {
  const toast = useToast();
  const queryClient = useQueryClient();
  const creating = channel === undefined;
  const scope = useProjectScope();
  const projects = scope.projects.data?.items;
  // A new channel starts in the project the shell is filtered to, or the Default project.
  const defaultProject = scope.projectId ?? projects?.find((p) => p.isDefault)?.id ?? "";
  const [values, setValues] = useState<ChannelFormValues>(
    channel ? valuesFromChannel(channel) : emptyChannelForm,
  );
  const [errors, setErrors] = useState<ChannelFormErrors>({});
  const [general, setGeneral] = useState<string | undefined>();
  // Editing keeps the stored webhook until the user asks to replace it.
  const [replacing, setReplacing] = useState(creating);
  const webhookInput = useRef<HTMLInputElement>(null);
  const nameInput = useRef<HTMLInputElement>(null);
  useEffect(() => {
    nameInput.current?.focus();
  }, []);
  useEffect(() => {
    if (replacing && !creating) webhookInput.current?.focus();
  }, [replacing, creating]);

  const save = useMutation({
    mutationFn: (v: ChannelFormValues) => {
      if (creating)
        return createChannel(toChannelCreate({ ...v, project: v.project || defaultProject }));
      // The project is sent only when it changes (a move, ADR-0102).
      const unchanged = v.project === valuesFromChannel(channel).project;
      return updateChannel(channel.id, toChannelUpdate(unchanged ? { ...v, project: "" } : v));
    },
    onSuccess: (saved) => {
      // The URL is a secret: it is never shown again after saving.
      setValues((v) => ({ ...v, webhookUrl: "" }));
      save.reset();
      toast(creating ? `Channel ${saved.name} added` : "Changes saved");
      onDone();
    },
    onError: (err) => {
      // A plan-limit refusal means the cached usage is stale.
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      const fields: ChannelFormErrors = {};
      if (err instanceof ApiError) {
        for (const [field, message] of Object.entries(err.fieldErrorMap())) {
          const raw = field.split(".")[0]!;
          const key = raw === "projectId" || raw === "organizationWide" ? "project" : raw;
          if (key in values) fields[key as keyof ChannelFormValues] ??= message;
        }
      }
      setErrors(fields);
      setGeneral(Object.keys(fields).length > 0 ? undefined : errorMessage(err));
    },
  });

  function update<K extends keyof ChannelFormValues>(key: K, value: ChannelFormValues[K]) {
    setValues((v) => ({ ...v, [key]: value }));
    setErrors((e) => (e[key] ? { ...e, [key]: undefined } : e));
  }

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const found = validateChannelForm(values, creating);
    setErrors(found);
    setGeneral(undefined);
    if (Object.keys(found).length === 0) save.mutate(values);
  }

  return (
    <form
      onSubmit={onSubmit}
      noValidate
      aria-labelledby="channel-form-heading"
      className="flex flex-col gap-5 border border-border p-4"
    >
      <h3 id="channel-form-heading" className="text-lead font-semibold">
        {creating ? "Add channel" : `Edit ${channel.name}`}
      </h3>
      {general && <Alert title="Could not save the channel.">{general}</Alert>}
      <fieldset disabled={save.isPending} className="flex min-w-0 flex-col gap-5">
        <Field
          label="Name"
          hint="Usually the Slack channel, such as #ops-alerts."
          error={errors.name}
        >
          {(c) => (
            <Input
              {...c}
              ref={nameInput}
              name="name"
              autoComplete="off"
              maxLength={CHANNEL_NAME_MAX * 2}
              placeholder="#ops-alerts"
              value={values.name}
              onChange={(e) => update("name", e.target.value)}
            />
          )}
        </Field>

        {replacing ? (
          <Field
            label={creating ? "Slack webhook URL" : "New Slack webhook URL"}
            hint={
              creating
                ? "Create an Incoming Webhook in Slack and paste its URL. It is stored encrypted and never shown again after saving."
                : "Replaces the stored webhook and reactivates a failed channel."
            }
            error={errors.webhookUrl}
          >
            {(c) => (
              <Input
                {...c}
                ref={webhookInput}
                name="webhookUrl"
                type="text"
                mono
                secret
                autoComplete="off"
                spellCheck={false}
                autoCapitalize="off"
                placeholder="https://hooks.slack.com/services/…"
                value={values.webhookUrl}
                onChange={(e) => update("webhookUrl", e.target.value)}
              />
            )}
          </Field>
        ) : (
          <WriteOnlyValue
            label="Webhook URL"
            hint="Stored encrypted. It cannot be shown again, only replaced."
            onReplace={() => setReplacing(true)}
          />
        )}

        <fieldset className="flex flex-col gap-1">
          <legend className="pb-1.5 text-caption font-medium">Events</legend>
          {channelEvents.map((e) => (
            <Checkbox
              key={e.value}
              name="events"
              value={e.value}
              label={e.label}
              checked={values.events.includes(e.value)}
              onChange={(ev) =>
                update(
                  "events",
                  ev.target.checked
                    ? [...values.events, e.value]
                    : values.events.filter((x) => x !== e.value),
                )
              }
            />
          ))}
          {errors.events && (
            <span role="alert" className="text-caption text-down">
              {errors.events}
            </span>
          )}
        </fieldset>

        {projects && projects.length > 0 && (
          <Field
            label="Project"
            hint="Monitors of this project can select the channel, and it gets their alerts when a monitor selects no channel. An organization-wide channel can be selected by every project's monitors. Moving it removes it from monitors of other projects."
            error={errors.project}
          >
            {(c) => (
              <Select
                {...c}
                name="project"
                value={values.project || defaultProject}
                onChange={(e) => update("project", e.target.value)}
              >
                {(projects ?? []).map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
                <option value={ORGANIZATION_WIDE}>Organization-wide</option>
              </Select>
            )}
          </Field>
        )}

        <Field
          label="Mention (optional)"
          hint="Up to 3 of @here, @channel, <!subteam^ID> (a user group) or <@UID> (a user), separated by spaces. Used in down alerts and reminders only."
          error={errors.mention}
        >
          {(c) => (
            <Input
              {...c}
              name="mention"
              mono
              autoComplete="off"
              spellCheck={false}
              autoCapitalize="off"
              placeholder="@here"
              value={values.mention}
              onChange={(e) => update("mention", e.target.value)}
            />
          )}
        </Field>

        <Field
          label="Reminder every (minutes)"
          hint={`0 turns reminders off. Otherwise 5 to ${REMINDER_MAX} minutes between reminders while a monitor stays down.`}
          error={errors.reminderMinutes}
        >
          {(c) => (
            <Input
              {...c}
              name="reminderMinutes"
              type="number"
              inputMode="numeric"
              min={0}
              max={REMINDER_MAX}
              mono
              end="min"
              className="sm:w-40"
              value={values.reminderMinutes}
              onChange={(e) => update("reminderMinutes", e.target.value)}
            />
          )}
        </Field>
      </fieldset>

      <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
        <Button type="submit" busy={save.isPending}>
          {save.isPending ? "Saving…" : creating ? "Add channel" : "Save channel"}
        </Button>
        <Button variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  );
}

function Appearance() {
  const [theme, setTheme] = useTheme();
  return (
    <div className="flex flex-col items-start gap-2">
      <Segmented
        label="Theme"
        name="theme"
        value={theme}
        onChange={setTheme}
        options={[
          { value: "system", label: "System" },
          { value: "light", label: "Light" },
          { value: "dark", label: "Dark" },
        ]}
      />
      <span className="text-caption text-muted-foreground">
        System follows your device&apos;s light or dark mode.
      </span>
    </div>
  );
}
