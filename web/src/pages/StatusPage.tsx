import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiError, errorMessage, errorMeta } from "../api/client";
import {
  clearStatusPagePassword,
  createStatusPage,
  deleteStatusPage,
  listMonitors,
  listStatusPageComponents,
  listStatusPages,
  POLL_MS,
  previewStatusPage,
  putStatusPageComponents,
  queryKeys,
  setStatusPagePassword,
  updateStatusPage,
  type StatusPage as Page,
  type StatusPageComponent,
  type StatusPageSnapshot,
  type StatusPageUpdate,
} from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import { useToast } from "../components/Toasts";
import {
  Alert,
  Badge,
  Button,
  buttonClass,
  ConfirmDialog,
  CopyField,
  cx,
  DiamondShape,
  DotShape,
  EmptyNote,
  ErrorState,
  Field,
  HollowDotShape,
  Input,
  Loading,
  Select,
  Skeleton,
  Textarea,
  TriangleShape,
  WriteOnlyValue,
} from "../components/ui";
import { abilities, ROLE_HINTS } from "../lib/roles";
import {
  accentError,
  COMPONENT_TEXT,
  COMPONENTS_MAX,
  DESCRIPTION_MAX,
  embedSnippets,
  formatUptime,
  groupComponents,
  hasInvisible,
  INVISIBLE_MESSAGE,
  logoError,
  move,
  NAME_MAX,
  OVERALL_TEXT,
  passwordError,
  slugError,
  slugFrom,
  TITLE_MAX,
  type PublicOverallStatus,
  type PublicStatus,
} from "../lib/statusPage";

/** The organization's public status page (P-05, ADR-0103): settings, components and a preview. */
export default function StatusPageSettings() {
  const can = abilities(useAuth().session);
  const pages = useQuery({ queryKey: queryKeys.statusPages, queryFn: listStatusPages });
  const page = pages.data?.items[0];

  return (
    <section className="flex flex-col gap-6">
      <div className="flex max-w-[760px] flex-col gap-2 pt-6 sm:pt-10">
        <h1 className="text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:text-display">
          Status page
        </h1>
        <p className="text-muted-foreground">
          A public page with the health of the services you choose: their state, 90 days of history
          and incidents. Only public names are shown, never monitor names, addresses or projects.
          Updates reach the page within a minute.
        </p>
        {!can.manageStatusPage && (
          <p className="text-caption text-muted-foreground">{ROLE_HINTS.manageStatusPage}</p>
        )}
      </div>
      {pages.isPending ? (
        <Loading label="Loading the status page…">
          <Skeleton className="w-2/3" />
          <Skeleton className="w-1/2" />
        </Loading>
      ) : pages.isError ? (
        <ErrorState
          title="Cannot load the status page"
          detail={errorMessage(pages.error)}
          meta={errorMeta(pages.error)}
          onRetry={() => void pages.refetch()}
        />
      ) : !page ? (
        can.manageStatusPage ? (
          <CreatePage />
        ) : (
          <EmptyNote>This organization has no status page yet.</EmptyNote>
        )
      ) : can.manageStatusPage ? (
        <Editor key={page.id} page={page} />
      ) : (
        <ReadOnly page={page} />
      )}
    </section>
  );
}

function fieldErrors(err: unknown): Record<string, string> {
  return err instanceof ApiError ? err.fieldErrorMap() : {};
}

function CreatePage() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [title, setTitle] = useState("");
  const [slug, setSlug] = useState("");
  const [slugTouched, setSlugTouched] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [formError, setFormError] = useState<string>();
  const create = useMutation({
    mutationFn: () => createStatusPage({ title: title.trim(), slug }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.statusPages });
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      toast("Status page created. Add components, then publish it.");
    },
    onError: (err) => {
      const fe = fieldErrors(err);
      setErrors(fe);
      setFormError(Object.keys(fe).length ? undefined : errorMessage(err));
    },
  });

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const next: Record<string, string> = {};
    if (!title.trim()) next.title = "Enter a title.";
    else if (hasInvisible(title)) next.title = INVISIBLE_MESSAGE;
    const se = slugError(slug);
    if (se) next.slug = se;
    setErrors(next);
    if (Object.keys(next).length === 0) create.mutate();
  }

  return (
    <form
      onSubmit={onSubmit}
      noValidate
      aria-label="Create the status page"
      className="flex max-w-[560px] flex-col gap-4 border border-border p-4 sm:p-5"
    >
      <h2 className="text-lead font-semibold">Create the status page</h2>
      {formError && <Alert>{formError}</Alert>}
      <Field label="Title" error={errors.title} hint="Shown at the top of the page.">
        {(c) => (
          <Input
            {...c}
            value={title}
            maxLength={TITLE_MAX}
            placeholder="Acme status"
            onChange={(e) => {
              setTitle(e.target.value);
              if (!slugTouched) setSlug(slugFrom(e.target.value));
            }}
          />
        )}
      </Field>
      <Field
        label="Address"
        error={errors.slug}
        hint="Lowercase letters, digits and hyphens. Addresses are shared by every organization."
      >
        {(c) => (
          <Input
            {...c}
            mono
            start="/status/"
            value={slug}
            autoComplete="off"
            onChange={(e) => {
              setSlugTouched(true);
              setSlug(e.target.value.toLowerCase());
            }}
          />
        )}
      </Field>
      <div>
        <Button type="submit" busy={create.isPending}>
          Create status page
        </Button>
      </div>
    </form>
  );
}

function PublishState({ page }: { page: Page }) {
  return (
    <span className="flex flex-wrap items-center gap-2">
      {page.published ? (
        <Badge tone="success" icon={<DotShape />}>
          Published
        </Badge>
      ) : (
        <Badge tone="neutral" icon={<HollowDotShape />}>
          Not published
        </Badge>
      )}
      {page.passwordProtected && <Badge tone="strong">Password</Badge>}
    </span>
  );
}

function ReadOnly({ page }: { page: Page }) {
  return (
    <div className="flex max-w-[640px] flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="text-lead font-semibold">{page.title}</h2>
        <PublishState page={page} />
      </div>
      {page.published ? (
        <CopyField label="Public link" value={page.publicUrl} once={false} />
      ) : (
        <p className="text-muted-foreground">The page is not published yet.</p>
      )}
    </div>
  );
}

function Editor({ page }: { page: Page }) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [branding, setBranding] = useState({
    title: page.title,
    slug: page.slug,
    description: page.description,
    logoUrl: page.logoUrl ?? "",
    accentColor: page.accentColor ?? "",
  });
  const preview = useQuery({
    queryKey: queryKeys.statusPagePreview(page.id),
    queryFn: () => previewStatusPage(page.id),
    refetchInterval: POLL_MS * 2,
  });
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.statusPages });
  };
  const publish = useMutation({
    mutationFn: (published: boolean) => updateStatusPage(page.id, { published }),
    onSuccess: (p) => {
      refresh();
      toast(p.published ? "Status page published" : "Status page unpublished");
    },
    onError: (err) => toast(`Change failed. ${errorMessage(err)}`, "error"),
  });

  return (
    <div className="grid gap-8 lg:grid-cols-[minmax(0,1fr)_minmax(0,440px)]">
      <div className="flex min-w-0 flex-col gap-8">
        <div className="flex flex-col gap-3 border border-border p-4 sm:p-5">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <PublishState page={page} />
            <span className="flex flex-wrap gap-2">
              {page.published && (
                <a
                  className={buttonClass("secondary", "small")}
                  href={page.publicUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  Open public page
                </a>
              )}
              <Button
                size="small"
                variant={page.published ? "secondary" : "primary"}
                busy={publish.isPending}
                onClick={() => publish.mutate(!page.published)}
              >
                {page.published ? "Unpublish" : "Publish"}
              </Button>
            </span>
          </div>
          <CopyField label="Public link" value={page.publicUrl} once={false} />
          {!page.published && (
            <p className="text-caption text-muted-foreground">
              Until it is published, the address answers “not found”, like an unknown page.
            </p>
          )}
        </div>
        <Branding page={page} value={branding} onChange={setBranding} />
        <Components page={page} />
        <Password page={page} />
        <Embed page={page} />
        <DangerZone page={page} />
      </div>
      <aside aria-label="Live preview" className="min-w-0 lg:sticky lg:top-6 lg:self-start">
        <h2 className="mb-2 eyebrow">Preview</h2>
        {preview.isPending ? (
          <Loading label="Loading the preview…">
            <Skeleton className="h-40 w-full" />
          </Loading>
        ) : preview.isError ? (
          <ErrorState
            variant="section"
            title="Cannot load the preview"
            detail={errorMessage(preview.error)}
            onRetry={() => void preview.refetch()}
          />
        ) : (
          <Preview snapshot={preview.data} branding={branding} />
        )}
      </aside>
    </div>
  );
}

interface BrandingValue {
  title: string;
  slug: string;
  description: string;
  logoUrl: string;
  accentColor: string;
}

function Branding({
  page,
  value,
  onChange,
}: {
  page: Page;
  value: BrandingValue;
  onChange: (v: BrandingValue) => void;
}) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [errors, setErrors] = useState<Record<string, string>>({});
  const set = (k: keyof BrandingValue) => (v: string) => {
    onChange({ ...value, [k]: v });
    setErrors((e) => ({ ...e, [k]: "" }));
  };
  const save = useMutation({
    mutationFn: (body: StatusPageUpdate) => updateStatusPage(page.id, body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.statusPages });
      void queryClient.invalidateQueries({ queryKey: queryKeys.statusPagePreview(page.id) });
      toast("Status page saved");
    },
    onError: (err) => {
      const fe = fieldErrors(err);
      setErrors(fe);
      if (Object.keys(fe).length === 0) toast(`Save failed. ${errorMessage(err)}`, "error");
    },
  });

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const next: Record<string, string> = {};
    if (!value.title.trim()) next.title = "Enter a title.";
    else if (hasInvisible(value.title)) next.title = INVISIBLE_MESSAGE;
    if (hasInvisible(value.description)) next.description = INVISIBLE_MESSAGE;
    const se = slugError(value.slug);
    if (se) next.slug = se;
    const le = logoError(value.logoUrl);
    if (le) next.logoUrl = le;
    const ae = accentError(value.accentColor);
    if (ae) next.accentColor = ae;
    if ([...value.description].length > DESCRIPTION_MAX)
      next.description = `Use at most ${DESCRIPTION_MAX} characters.`;
    setErrors(next);
    if (Object.keys(next).length > 0) return;
    const body: StatusPageUpdate = {};
    if (value.title.trim() !== page.title) body.title = value.title.trim();
    if (value.slug !== page.slug) body.slug = value.slug;
    if (value.description.trim() !== page.description) body.description = value.description.trim();
    if (value.logoUrl.trim() !== (page.logoUrl ?? "")) body.logoUrl = value.logoUrl.trim();
    if (value.accentColor.trim().toLowerCase() !== (page.accentColor ?? ""))
      body.accentColor = value.accentColor.trim();
    if (Object.keys(body).length === 0) {
      toast("Nothing to save");
      return;
    }
    save.mutate(body);
  }

  return (
    <form
      onSubmit={onSubmit}
      noValidate
      aria-labelledby="branding-h"
      className="flex flex-col gap-4"
    >
      <h2 id="branding-h" className="text-lead font-semibold">
        Page
      </h2>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Title" error={errors.title}>
          {(c) => (
            <Input
              {...c}
              value={value.title}
              maxLength={TITLE_MAX}
              onChange={(e) => set("title")(e.target.value)}
            />
          )}
        </Field>
        <Field
          label="Address"
          error={errors.slug}
          hint={
            value.slug !== page.slug ? "Changing it breaks links to the old address." : undefined
          }
        >
          {(c) => (
            <Input
              {...c}
              mono
              start="/status/"
              autoComplete="off"
              value={value.slug}
              onChange={(e) => set("slug")(e.target.value.toLowerCase())}
            />
          )}
        </Field>
      </div>
      <Field label="Description" error={errors.description} hint="Optional, plain text.">
        {(c) => (
          <Textarea
            {...c}
            rows={3}
            value={value.description}
            onChange={(e) => set("description")(e.target.value)}
          />
        )}
      </Field>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field
          label="Logo URL"
          error={errors.logoUrl}
          hint="https only. Visitors' browsers load it; Maiak never fetches it."
        >
          {(c) => (
            <Input
              {...c}
              mono
              inputMode="url"
              placeholder="https://cdn.example.com/logo.svg"
              value={value.logoUrl}
              onChange={(e) => set("logoUrl")(e.target.value)}
            />
          )}
        </Field>
        <Field
          label="Accent colour"
          error={errors.accentColor}
          hint="A rule under the title and link hovers. Status colours stay fixed."
        >
          {(c) => (
            <Input
              {...c}
              mono
              placeholder="#1f6feb"
              maxLength={7}
              value={value.accentColor}
              onChange={(e) => set("accentColor")(e.target.value)}
            />
          )}
        </Field>
      </div>
      <div>
        <Button type="submit" busy={save.isPending}>
          Save page
        </Button>
      </div>
    </form>
  );
}

interface Row {
  monitorId: string;
  publicName: string;
  groupName: string;
}

function toRows(items: StatusPageComponent[]): Row[] {
  return items.map((c) => ({
    monitorId: c.monitorId,
    publicName: c.publicName,
    groupName: c.groupName ?? "",
  }));
}

function Components({ page }: { page: Page }) {
  const components = useQuery({
    queryKey: queryKeys.statusPageComponents(page.id),
    queryFn: () => listStatusPageComponents(page.id),
  });
  return (
    <section aria-labelledby="components-h" className="flex flex-col gap-3">
      <h2 id="components-h" className="text-lead font-semibold">
        Components
      </h2>
      <p className="text-caption text-muted-foreground">
        Monitors and heartbeats from any project. Visitors see only the public name and group, the
        state and the daily history. Components with the same group next to each other are shown
        together.
      </p>
      {components.isPending ? (
        <Loading label="Loading components…" />
      ) : components.isError ? (
        <ErrorState
          variant="section"
          title="Cannot load components"
          detail={errorMessage(components.error)}
          onRetry={() => void components.refetch()}
        />
      ) : (
        <ComponentEditor page={page} initial={components.data.items} />
      )}
    </section>
  );
}

function ComponentEditor({ page, initial }: { page: Page; initial: StatusPageComponent[] }) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const monitors = useQuery({
    queryKey: queryKeys.monitorList(undefined),
    queryFn: () => listMonitors(),
  });
  const [rows, setRows] = useState<Row[]>(() => toRows(initial));
  const [pick, setPick] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const names = new Map<string, string>();
  for (const c of initial) names.set(c.monitorId, `${c.monitorName} · ${c.monitorType}`);
  for (const m of monitors.data?.items ?? []) names.set(m.id, `${m.name} · ${m.type}`);
  const used = new Set(rows.map((r) => r.monitorId));
  const available = (monitors.data?.items ?? []).filter((m) => !used.has(m.id));
  const dirty = JSON.stringify(rows) !== JSON.stringify(toRows(initial));

  const save = useMutation({
    mutationFn: () =>
      putStatusPageComponents(
        page.id,
        rows.map((r) => ({
          monitorId: r.monitorId,
          publicName: r.publicName.trim(),
          ...(r.groupName.trim() ? { groupName: r.groupName.trim() } : {}),
        })),
      ),
    onSuccess: (list) => {
      setRows(toRows(list.items));
      queryClient.setQueryData(queryKeys.statusPageComponents(page.id), list);
      void queryClient.invalidateQueries({ queryKey: queryKeys.statusPages });
      void queryClient.invalidateQueries({ queryKey: queryKeys.statusPagePreview(page.id) });
      toast("Components saved");
    },
    onError: (err) => {
      const fe = fieldErrors(err);
      setErrors(fe);
      if (Object.keys(fe).length === 0) toast(`Save failed. ${errorMessage(err)}`, "error");
    },
  });

  function update(i: number, patch: Partial<Row>) {
    setRows(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
    setErrors({});
  }

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const next: Record<string, string> = {};
    rows.forEach((r, i) => {
      if (!r.publicName.trim()) next[`items.${i}.publicName`] = "Enter a public name.";
      else if (hasInvisible(r.publicName)) next[`items.${i}.publicName`] = INVISIBLE_MESSAGE;
      else if ([...r.publicName.trim()].length > NAME_MAX)
        next[`items.${i}.publicName`] = `Use ${NAME_MAX} characters or fewer.`;
      if (hasInvisible(r.groupName)) next[`items.${i}.groupName`] = INVISIBLE_MESSAGE;
      else if ([...r.groupName.trim()].length > NAME_MAX)
        next[`items.${i}.groupName`] = `Use ${NAME_MAX} characters or fewer.`;
    });
    setErrors(next);
    if (Object.keys(next).length === 0) save.mutate();
  }

  return (
    <form onSubmit={onSubmit} noValidate aria-label="Components" className="flex flex-col gap-3">
      {rows.length === 0 ? (
        <EmptyNote>No components yet. The public page says “No components.”</EmptyNote>
      ) : (
        <ol className="flex flex-col border border-border">
          {rows.map((r, i) => (
            <li
              key={r.monitorId}
              className="flex flex-col gap-2 border-t border-border-faint p-3 first:border-t-0"
            >
              <div className="flex items-center justify-between gap-2">
                <span className="min-w-0 truncate font-mono text-mono text-muted-foreground">
                  {i + 1}. {names.get(r.monitorId) ?? "Monitor"}
                </span>
                <span className="flex flex-none gap-1">
                  <Button
                    type="button"
                    variant="ghost"
                    size="small"
                    disabled={i === 0}
                    aria-label={`Move ${r.publicName || "component"} up`}
                    onClick={() => setRows(move(rows, i, -1))}
                  >
                    ↑
                  </Button>
                  <Button
                    type="button"
                    variant="ghost"
                    size="small"
                    disabled={i === rows.length - 1}
                    aria-label={`Move ${r.publicName || "component"} down`}
                    onClick={() => setRows(move(rows, i, 1))}
                  >
                    ↓
                  </Button>
                  <Button
                    type="button"
                    variant="ghost-destructive"
                    size="small"
                    aria-label={`Remove ${r.publicName || "component"}`}
                    onClick={() => setRows(rows.filter((_, j) => j !== i))}
                  >
                    Remove
                  </Button>
                </span>
              </div>
              <div className="grid gap-2 sm:grid-cols-2">
                <Field label="Public name" error={errors[`items.${i}.publicName`]}>
                  {(c) => (
                    <Input
                      {...c}
                      value={r.publicName}
                      maxLength={NAME_MAX * 2}
                      placeholder="API"
                      onChange={(e) => update(i, { publicName: e.target.value })}
                    />
                  )}
                </Field>
                <Field label="Group (optional)" error={errors[`items.${i}.groupName`]}>
                  {(c) => (
                    <Input
                      {...c}
                      value={r.groupName}
                      maxLength={NAME_MAX * 2}
                      placeholder="No group"
                      onChange={(e) => update(i, { groupName: e.target.value })}
                    />
                  )}
                </Field>
              </div>
              {errors[`items.${i}.monitorId`] && (
                <span className="text-caption text-down">{errors[`items.${i}.monitorId`]}</span>
              )}
            </li>
          ))}
        </ol>
      )}
      <div className="flex flex-col gap-2 sm:flex-row sm:items-end">
        <Field label="Add a monitor or heartbeat" className="sm:w-80">
          {(c) => (
            <Select {...c} value={pick} onChange={(e) => setPick(e.target.value)}>
              <option value="">
                {monitors.isPending
                  ? "Loading monitors…"
                  : available.length
                    ? "Choose…"
                    : "None left"}
              </option>
              {available.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name} ({m.type === "heartbeat" ? "heartbeat" : "HTTPS"})
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Button
          type="button"
          variant="secondary"
          disabled={!pick || rows.length >= COMPONENTS_MAX}
          onClick={() => {
            setRows([...rows, { monitorId: pick, publicName: "", groupName: "" }]);
            setPick("");
          }}
        >
          Add component
        </Button>
      </div>
      <p className="text-caption text-muted-foreground">
        New components start without a name on purpose: choose one that says nothing internal.
      </p>
      <div className="flex items-center gap-3">
        <Button type="submit" busy={save.isPending} disabled={!dirty}>
          Save components
        </Button>
        {dirty && <span className="text-caption text-muted-foreground">Unsaved changes</span>}
      </div>
    </form>
  );
}

function Password({ page }: { page: Page }) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [editing, setEditing] = useState(!page.passwordProtected);
  const [value, setValue] = useState("");
  const [error, setError] = useState<string>();
  const [clearing, setClearing] = useState(false);
  const done = (msg: string) => {
    setValue("");
    void queryClient.invalidateQueries({ queryKey: queryKeys.statusPages });
    toast(msg);
  };
  const set = useMutation({
    mutationFn: () => setStatusPagePassword(page.id, value),
    onSuccess: () => {
      setEditing(false);
      done("Password set. Visitors must enter it.");
    },
    onError: (err) => setError(fieldErrors(err).password ?? errorMessage(err)),
  });
  const clear = useMutation({
    mutationFn: () => clearStatusPagePassword(page.id),
    onSuccess: () => {
      setClearing(false);
      setEditing(true);
      done("Password removed");
    },
    onError: (err) => {
      setClearing(false);
      toast(`Remove failed. ${errorMessage(err)}`, "error");
    },
  });

  return (
    <section aria-labelledby="password-h" className="flex flex-col gap-3">
      <h2 id="password-h" className="text-lead font-semibold">
        Password
      </h2>
      <p className="text-caption text-muted-foreground">
        Optional. A protected page asks for the password once per browser (12 hours) and has no
        badge, JSON or RSS feed. Changing the password signs every visitor out. Setting, changing or
        removing it applies within about a minute: until then visitors and caches may still see the
        previous state.
      </p>
      {page.passwordProtected && !editing ? (
        <div className="flex flex-col gap-2">
          <WriteOnlyValue label="Password" onReplace={() => setEditing(true)} />
          <div>
            <Button variant="ghost-destructive" size="small" onClick={() => setClearing(true)}>
              Remove password
            </Button>
          </div>
        </div>
      ) : (
        <form
          noValidate
          aria-label="Set a password"
          className="flex flex-col gap-2 sm:flex-row sm:items-end"
          onSubmit={(e) => {
            e.preventDefault();
            const pe = passwordError(value);
            setError(pe);
            if (!pe) set.mutate();
          }}
        >
          <Field label="New password" error={error} className="sm:w-80">
            {(c) => (
              <Input
                {...c}
                type="password"
                autoComplete="new-password"
                value={value}
                onChange={(e) => {
                  setValue(e.target.value);
                  setError(undefined);
                }}
              />
            )}
          </Field>
          <span className="flex gap-2">
            <Button type="submit" busy={set.isPending} className={error ? "sm:mb-7" : undefined}>
              Set password
            </Button>
            {page.passwordProtected && (
              <Button type="button" variant="secondary" onClick={() => setEditing(false)}>
                Cancel
              </Button>
            )}
          </span>
        </form>
      )}
      {clearing && (
        <ConfirmDialog
          title="Remove the password?"
          confirmLabel="Remove password"
          busy={clear.isPending}
          onConfirm={() => clear.mutate()}
          onCancel={() => setClearing(false)}
        >
          <p>
            Anyone with the link can open the page again, and its badge, JSON and RSS work, within
            about a minute.
          </p>
        </ConfirmDialog>
      )}
    </section>
  );
}

function Embed({ page }: { page: Page }) {
  const { script, iframe } = embedSnippets(page.publicUrl);
  return (
    <section aria-labelledby="embed-h" className="flex flex-col gap-3">
      <h2 id="embed-h" className="text-lead font-semibold">
        Embed and feeds
      </h2>
      {page.passwordProtected ? (
        <p className="text-muted-foreground">
          A protected page has no badge, JSON or RSS feed. Remove the password to use them.
        </p>
      ) : (
        <>
          <p className="text-caption text-muted-foreground">
            A small badge with the overall state that links to the page. It works once the page is
            published.
          </p>
          <CopyField label="Badge script" value={script} once={false} />
          <CopyField label="Badge iframe" value={iframe} once={false} />
          <CopyField
            label="RSS feed of incidents"
            value={`${page.publicUrl}/feed.rss`}
            once={false}
          />
          <CopyField label="JSON" value={`${page.publicUrl}.json`} once={false} />
        </>
      )}
    </section>
  );
}

function DangerZone({ page }: { page: Page }) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => deleteStatusPage(page.id),
    onSuccess: () => {
      setConfirming(false);
      void queryClient.invalidateQueries({ queryKey: queryKeys.statusPages });
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      toast("Status page deleted");
    },
    onError: (err) => {
      setConfirming(false);
      toast(`Delete failed. ${errorMessage(err)}`, "error");
    },
  });
  return (
    <section className="border-t border-border pt-4">
      <Button variant="ghost-destructive" onClick={() => setConfirming(true)}>
        Delete status page
      </Button>
      {confirming && (
        <ConfirmDialog
          title="Delete the status page?"
          confirmLabel="Delete status page"
          busy={remove.isPending}
          onConfirm={() => remove.mutate()}
          onCancel={() => setConfirming(false)}
        >
          <p>
            The public address answers “not found” at once and becomes free for others. Monitors are
            not touched. You can create a new page afterwards.
          </p>
        </ConfirmDialog>
      )}
    </section>
  );
}

const BAR: Record<PublicStatus, string> = {
  operational: "bg-success",
  degraded: "bg-warning",
  outage: "bg-destructive",
  no_data: "bg-border",
};

function StateMark({ status }: { status: PublicStatus | PublicOverallStatus }) {
  switch (status) {
    case "operational":
      return <DotShape className="text-success" />;
    case "degraded":
      return <DiamondShape className="text-warning" />;
    case "outage":
    case "partial_outage":
    case "major_outage":
      return <TriangleShape className="text-down" />;
    default:
      return <HollowDotShape />;
  }
}

/**
 * The public page drawn from the preview snapshot with the unsaved branding on top. The logo is
 * not loaded here (the app's policy allows images only from itself); its host is shown instead.
 */
function Preview({
  snapshot,
  branding,
}: {
  snapshot: StatusPageSnapshot;
  branding: BrandingValue;
}) {
  const accent =
    accentError(branding.accentColor) || !branding.accentColor ? undefined : branding.accentColor;
  let logoHost: string | undefined;
  if (branding.logoUrl && !logoError(branding.logoUrl)) logoHost = new URL(branding.logoUrl).host;
  const groups = groupComponents(snapshot.components);
  const overall = snapshot.status;
  return (
    <div
      className="flex flex-col gap-4 border border-border border-t-[3px] bg-background p-4"
      style={accent ? { borderTopColor: accent } : undefined}
    >
      <div className="flex min-w-0 items-center gap-3">
        {logoHost && (
          <span className="flex-none border border-dashed border-input px-1.5 py-1 font-mono text-micro text-muted-foreground">
            logo · {logoHost}
          </span>
        )}
        <span className="min-w-0 text-title font-semibold break-words">
          {branding.title.trim() || snapshot.page.title}
        </span>
      </div>
      {branding.description.trim() && (
        <p className="text-caption whitespace-pre-line text-muted-foreground">
          {branding.description.trim()}
        </p>
      )}
      <div
        className={cx(
          "flex items-center gap-2 border px-3 py-2.5 font-semibold",
          overall === "operational" && "border-border",
          overall === "degraded" && "border-warning-border bg-warning-tint",
          (overall === "partial_outage" || overall === "major_outage") &&
            "border-down-border bg-down-tint",
          overall === "no_data" && "border-border bg-muted",
        )}
      >
        <StateMark status={overall} />
        {OVERALL_TEXT[overall]}
      </div>
      {snapshot.activeIncidents.length > 0 && (
        <ul className="flex flex-col gap-1">
          {snapshot.activeIncidents.map((i) => (
            <li key={i.id} className="flex items-center gap-2 text-caption">
              <TriangleShape size="small" className="text-down" />
              {i.title}
            </li>
          ))}
        </ul>
      )}
      {groups.length === 0 ? (
        <p className="text-caption text-muted-foreground">No components.</p>
      ) : (
        groups.map((g, gi) => (
          <div key={gi} className="flex flex-col border border-border">
            {g.name && (
              <div className="border-b border-border-faint bg-muted px-3 py-1.5 eyebrow">
                {g.name}
              </div>
            )}
            {g.items.map((c, ci) => (
              <div
                key={ci}
                className="flex flex-col gap-1.5 border-t border-border-faint px-3 py-2.5 first:border-t-0"
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="min-w-0 truncate font-medium">{c.name}</span>
                  <span className="flex flex-none items-center gap-1.5 text-caption text-muted-foreground">
                    <StateMark status={c.status} />
                    {COMPONENT_TEXT[c.status]}
                  </span>
                </div>
                <div
                  aria-hidden="true"
                  className="grid h-5 grid-cols-[repeat(90,minmax(0,1fr))] gap-px"
                >
                  {c.days.map((d) => (
                    <span
                      key={d.date}
                      className={BAR[d.status]}
                      title={`${d.date}: ${formatUptime(d.uptime)}`}
                    />
                  ))}
                </div>
                <span className="font-mono text-mono text-subtle-foreground">
                  90 days · {formatUptime(c.uptime)}
                </span>
              </div>
            ))}
          </div>
        ))
      )}
      <p className="text-caption text-muted-foreground">
        {snapshot.pastIncidents.length === 0
          ? "No incidents in the last 90 days."
          : `${snapshot.pastIncidents.length} past incident${snapshot.pastIncidents.length === 1 ? "" : "s"} in the last 90 days.`}
      </p>
    </div>
  );
}
