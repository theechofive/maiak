import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ApiError, errorMessage, errorMeta, planLimitMessage } from "../api/client";
import {
  createMonitor,
  getMonitor,
  listChannels,
  queryKeys,
  updateMonitor,
  type Channel,
  type MonitorDetail,
} from "../api/endpoints";
import HeartbeatUrlPanel from "../components/HeartbeatUrlPanel";
import MonitorNotFound from "../components/MonitorNotFound";
import { useToast } from "../components/Toasts";
import {
  Alert,
  Badge,
  Button,
  buttonClass,
  Checkbox,
  cx,
  DiamondShape,
  ErrorState,
  Field,
  Input,
  ReadOnlyValue,
  Segmented,
  Skeleton,
  Loading,
} from "../components/ui";
import { formatDuration } from "../lib/format";
import { GRACE_MAX, INTERVAL_MAX, INTERVAL_MIN, INTERVAL_PRESETS } from "../lib/heartbeat";
import {
  describeSetup,
  emptyMonitorForm,
  keepLiveChannels,
  mapServerErrors,
  NAME_MAX,
  toCreateRequest,
  toMonitorRequest,
  URL_MAX,
  validateMonitorForm,
  valuesFromMonitor,
  withHttps,
  withoutHttps,
  type FormField,
  type MonitorFormErrors,
  type MonitorFormValues,
} from "../lib/monitorForm";
import { atHeartbeatLimit, atMonitorLimit, useUsage } from "../lib/usage";

const title = "text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:text-display";

export default function MonitorForm({ mode }: { mode: "create" | "edit" }) {
  const { id = "" } = useParams();
  const monitor = useQuery({
    queryKey: queryKeys.monitor(id),
    queryFn: () => getMonitor(id),
    enabled: mode === "edit",
  });

  if (mode === "create") {
    return (
      <Page back={<Link to="/">← Monitors</Link>} title="Add monitor">
        <MonitorFormBody mode="create" initial={emptyMonitorForm} />
      </Page>
    );
  }
  if (monitor.isPending) {
    return (
      <Loading label="Loading the monitor…" className="pt-10">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="w-80 max-w-full" />
      </Loading>
    );
  }
  if (monitor.isError) {
    return monitor.error instanceof ApiError && monitor.error.status === 404 ? (
      <MonitorNotFound />
    ) : (
      <div className="pt-10">
        <ErrorState
          title="Cannot load the monitor"
          detail={errorMessage(monitor.error)}
          meta={errorMeta(monitor.error)}
          onRetry={() => void monitor.refetch()}
        />
      </div>
    );
  }
  const m = monitor.data;
  return (
    <Page
      back={<Link to={`/monitors/${m.id}`}>← {m.name}</Link>}
      title={`Edit ${m.name}`}
      note={
        <span className="flex flex-wrap items-center gap-2.5 text-caption text-muted-foreground">
          <Badge tone="strong">{m.type === "heartbeat" ? "Watchdog" : "HTTP"}</Badge>
          The type cannot change after a monitor is created.
        </span>
      }
    >
      <MonitorFormBody key={m.id} mode="edit" id={m.id} initial={valuesFromMonitor(m)} />
    </Page>
  );
}

function Page({
  back,
  title: heading,
  note,
  children,
}: {
  back: ReactNode;
  title: string;
  note?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="flex max-w-[640px] flex-col gap-7 pt-4">
      <div className="flex flex-col gap-2.5">
        <span className="self-start py-2.5 text-[13px] text-muted-foreground hover:text-foreground sm:py-0 [&_a]:hover:text-foreground">
          {back}
        </span>
        <h1 className={cx(title, "break-words")}>{heading}</h1>
        {note}
      </div>
      {children}
    </section>
  );
}

function MonitorFormBody({
  mode,
  id,
  initial,
}: {
  mode: "create" | "edit";
  id?: string;
  initial: MonitorFormValues;
}) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const toast = useToast();
  const [values, setValues] = useState(initial);
  const [errors, setErrors] = useState<MonitorFormErrors>({});
  const [general, setGeneral] = useState<string | undefined>();
  // A new heartbeat: its URL is shown once, here, before moving on.
  const [created, setCreated] = useState<MonitorDetail | undefined>();
  const usage = useUsage();
  // Shared with the channel picker (same query key). A selected channel that was deleted since
  // the monitor was loaded is dropped before saving, instead of failing with 422.
  const channels = useQuery({ queryKey: queryKeys.channels, queryFn: listChannels });

  const save = useMutation({
    mutationFn: (v: MonitorFormValues) => {
      const saved = keepLiveChannels(v, channels.data?.items);
      return mode === "create"
        ? createMonitor(toCreateRequest(saved))
        : updateMonitor(id!, toMonitorRequest(saved));
    },
    onSuccess: async (saved) => {
      if (mode === "create") void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      await queryClient.invalidateQueries({ queryKey: queryKeys.monitors });
      toast(mode === "create" ? "Monitor created" : "Changes saved");
      if (saved.heartbeatUrl) {
        setCreated(saved);
        return;
      }
      navigate(`/monitors/${saved.id}`);
    },
    onError: (err) => {
      // A plan-limit refusal means the cached usage is stale.
      if (planLimitMessage(err)) void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      const mapped = mapServerErrors(err);
      setErrors(mapped.fields);
      setGeneral(mapped.general);
    },
  });

  function update<K extends keyof MonitorFormValues>(key: K, value: MonitorFormValues[K]) {
    setValues((v) => ({ ...v, [key]: value }));
    const field: FormField =
      key === "expectedMode" || key === "expectedCode" ? "expectedStatus" : (key as FormField);
    setErrors((e) => (e[field] ? { ...e, [field]: undefined } : e));
  }

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const found = validateMonitorForm(values);
    setErrors(found);
    setGeneral(undefined);
    if (Object.keys(found).length > 0) {
      // Focus the first field that needs a change, in the order the form shows them.
      const first = (
        [
          "name",
          "url",
          "expectedStatus",
          "expectedIntervalSeconds",
          "graceSeconds",
          "notifySlack",
          "channelIds",
        ] as const
      ).find((f) => found[f]);
      if (first) document.querySelector<HTMLElement>(`[data-field="${first}"]`)?.focus();
      return;
    }
    save.mutate(values);
  }

  if (created?.heartbeatUrl) {
    return (
      <HeartbeatUrlPanel
        url={created.heartbeatUrl}
        expectedIntervalSeconds={created.expectedIntervalSeconds ?? created.intervalSeconds}
      >
        <Link to={`/monitors/${created.id}`} className={buttonClass()}>
          I saved the URL, open the monitor
        </Link>
      </HeartbeatUrlPanel>
    );
  }

  const heartbeat = values.type === "heartbeat";
  const hasErrors = Object.values(errors).some(Boolean);
  const cancelTo = mode === "edit" && id ? `/monitors/${id}` : "/";
  const scheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(withoutHttps(values.url));

  return (
    <form onSubmit={onSubmit} noValidate className="flex flex-col gap-7">
      {(general || hasErrors) && (
        <Alert title={mode === "create" ? "Could not create the monitor." : "Could not save."}>
          {general ?? "Check the highlighted fields."}
        </Alert>
      )}

      <fieldset disabled={save.isPending} className="flex min-w-0 flex-col gap-6">
        {mode === "create" && (
          <TypeChoice
            value={values.type}
            onChange={(type) => update("type", type)}
            httpFull={atMonitorLimit(usage.data)}
            heartbeatFull={atHeartbeatLimit(usage.data)}
          />
        )}

        <Field label="Name" error={errors.name}>
          {(c) => (
            <Input
              {...c}
              data-field="name"
              name="name"
              autoComplete="off"
              maxLength={NAME_MAX * 2}
              placeholder={heartbeat ? "Nightly backup" : "Checkout API"}
              value={values.name}
              onChange={(e) => update("name", e.target.value)}
            />
          )}
        </Field>

        {heartbeat ? (
          <HeartbeatFields values={values} update={update} errors={errors} />
        ) : (
          <>
            <Field
              label="URL"
              hint="HTTPS only. Private and internal addresses are not allowed."
              error={errors.url}
            >
              {(c) => (
                <Input
                  {...c}
                  data-field="url"
                  name="url"
                  mono
                  inputMode="url"
                  spellCheck={false}
                  autoCapitalize="off"
                  autoComplete="off"
                  maxLength={URL_MAX}
                  placeholder="api.example.com/health"
                  start={<span className={scheme ? "hidden" : undefined}>https://</span>}
                  value={withoutHttps(values.url)}
                  onChange={(e) => update("url", withHttps(e.target.value))}
                />
              )}
            </Field>

            <div className="flex flex-col gap-1.5">
              <div className="grid grid-cols-2 gap-3">
                <Field label="Check every">{() => <ReadOnlyValue>60 s</ReadOnlyValue>}</Field>
                <Field label="Timeout">{() => <ReadOnlyValue>10 s</ReadOnlyValue>}</Field>
              </div>
              <span className="text-caption text-muted-foreground">Fixed for now.</span>
            </div>

            <ExpectedStatus values={values} update={update} error={errors.expectedStatus} />

            <div className="flex flex-col gap-1.5">
              <span className="text-caption font-medium">Alert after</span>
              <div className="flex flex-wrap items-center gap-3">
                <Segmented
                  label="Alert after"
                  name="failureThreshold"
                  value={String(values.failureThreshold)}
                  onChange={(n) => update("failureThreshold", Number(n))}
                  options={["1", "2", "3"].map((n) => ({ value: n, label: n }))}
                />
                <span>
                  {values.failureThreshold === 1 ? "failed check" : "failed checks in a row"}
                </span>
              </div>
              <span className="text-caption text-muted-foreground">
                {errors.failureThreshold ??
                  (values.failureThreshold === 1
                    ? "One failed check is enough, so a single network blip alerts."
                    : `With a check every minute, that is about ${values.failureThreshold} minutes of failures before anyone hears.`)}
              </span>
            </div>
          </>
        )}

        <Checkbox
          name="notifySlack"
          label="Notify Slack"
          description={
            heartbeat
              ? "Slack gets a message when no ping arrives in time and when pings resume."
              : "Slack gets a message when it goes down and when it recovers, and a warning when the TLS certificate expires in 14 days or less."
          }
          data-field="notifySlack"
          error={errors.notifySlack}
          checked={values.notifySlack}
          onChange={(e) => update("notifySlack", e.target.checked)}
        />

        {values.notifySlack && (
          <ChannelPicker
            channels={channels}
            selected={values.channelIds}
            onChange={(ids) => update("channelIds", ids)}
            error={errors.channelIds}
          />
        )}
      </fieldset>

      <div className="flex flex-col gap-4 border-t border-border pt-5">
        <div className="flex flex-col gap-1.5">
          <span className="eyebrow">In plain English</span>
          <p aria-live="polite" className="text-[17px] leading-[1.5] max-sm:text-[16px]">
            {describeSetup(values).map((part, i) => (
              <span
                key={i}
                className={cx(
                  part.blank &&
                    "text-subtle-foreground underline decoration-dashed underline-offset-[5px]",
                )}
              >
                {part.text}
              </span>
            ))}
          </p>
        </div>
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
          <Button type="submit" busy={save.isPending}>
            {mode === "create"
              ? save.isPending
                ? "Creating…"
                : "Create monitor"
              : save.isPending
                ? "Saving…"
                : "Save changes"}
          </Button>
          <Link to={cancelTo} className={buttonClass("ghost")}>
            Cancel
          </Link>
        </div>
      </div>
    </form>
  );
}

/** Which notification channels get the monitor's alerts; none selected means all of them. */
function ChannelPicker({
  channels,
  selected,
  onChange,
  error,
}: {
  channels: {
    isPending: boolean;
    isError: boolean;
    data?: { items: Channel[] };
    refetch: () => unknown;
  };
  selected: string[];
  onChange: (ids: string[]) => void;
  error?: string;
}) {
  if (channels.isPending) {
    return (
      <Loading label="Loading notification channels…">
        <Skeleton className="w-1/3" />
      </Loading>
    );
  }
  if (channels.isError || !channels.data) {
    // Quiet on purpose: the monitor can still be saved, and the server decides about the ids.
    return (
      <p className="text-caption text-muted-foreground">
        Cannot load the notification channels; the current selection is kept.{" "}
        <Button variant="link" onClick={() => void channels.refetch()}>
          Retry
        </Button>
      </p>
    );
  }
  const items = channels.data.items;
  if (items.length === 0) {
    return (
      <p className="text-caption text-muted-foreground">
        No notification channels yet: add one in{" "}
        <Link to="/settings#channels-heading" className="underline underline-offset-3">
          Settings
        </Link>
        .
      </p>
    );
  }
  return (
    <fieldset
      data-field="channelIds"
      aria-describedby="channels-hint"
      className="flex flex-col gap-1"
    >
      <legend className="pb-1.5 text-caption font-medium">Notification channels</legend>
      {items.map((c) => (
        <Checkbox
          key={c.id}
          name="channelIds"
          value={c.id}
          label={c.name}
          description={
            c.status === "failed"
              ? "Webhook failed: nothing is sent until it is replaced."
              : undefined
          }
          checked={selected.includes(c.id)}
          onChange={(e) =>
            onChange(e.target.checked ? [...selected, c.id] : selected.filter((id) => id !== c.id))
          }
        />
      ))}
      {error && (
        <span role="alert" className="text-caption text-down">
          {error}
        </span>
      )}
      <span id="channels-hint" className="text-caption text-muted-foreground">
        None selected: every active channel gets the alerts.
      </span>
    </fieldset>
  );
}

function TypeChoice({
  value,
  onChange,
  httpFull,
  heartbeatFull,
}: {
  value: MonitorFormValues["type"];
  onChange: (type: MonitorFormValues["type"]) => void;
  httpFull: boolean;
  heartbeatFull: boolean;
}) {
  const options = [
    {
      type: "http" as const,
      title: "HTTP check",
      text: "Maiak calls your HTTPS endpoint every minute and checks the answer.",
      full: httpFull ? "Your plan's HTTP monitor limit is reached." : undefined,
    },
    {
      type: "heartbeat" as const,
      title: "Watchdog (heartbeat)",
      text: "Your job or Alertmanager's always-firing Watchdog alert pings a secret URL; Maiak alerts when the pings stop.",
      full: heartbeatFull ? "Your plan's watchdog limit is reached." : undefined,
    },
  ];
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className="pb-2 text-caption font-medium">What to monitor</legend>
      <div className="grid gap-2 sm:grid-cols-2">
        {options.map((o) => (
          <TypeCard
            key={o.type}
            {...o}
            checked={value === o.type}
            onPick={() => onChange(o.type)}
          />
        ))}
      </div>
    </fieldset>
  );
}

function TypeCard({
  type,
  title: heading,
  text,
  full,
  checked,
  onPick,
}: {
  type: string;
  title: string;
  text: string;
  full?: string;
  checked: boolean;
  onPick: () => void;
}) {
  const id = useId();
  return (
    <div
      className={cx(
        "relative flex items-start gap-2.5 border px-3.5 py-3 has-focus-visible:outline-2 has-focus-visible:outline-offset-2 has-focus-visible:outline-ring",
        checked
          ? "border-foreground bg-field shadow-[inset_0_0_0_1px_var(--foreground)]"
          : "border-input",
        full && !checked && "bg-muted",
      )}
    >
      {/* eslint-disable-next-line no-restricted-syntax -- the native radio of a one-off radio card; the card is the control. */}
      <input
        id={id}
        type="radio"
        name="type"
        value={type}
        checked={checked}
        onChange={onPick}
        aria-describedby={`${id}-text${full ? ` ${id}-full` : ""}`}
        className="mt-0.5 size-3.5 flex-none accent-foreground outline-none"
      />
      <span className="flex min-w-0 flex-col gap-1">
        {/* The label stretches over the card, so the whole card picks the type. */}
        <label
          htmlFor={id}
          className="cursor-pointer text-[14px] font-semibold after:absolute after:inset-0"
        >
          {heading}
        </label>
        <span id={`${id}-text`} className="text-caption text-muted-foreground">
          {text}
        </span>
        {full && (
          <span
            id={`${id}-full`}
            className="mt-1 flex items-center gap-1.5 border-t border-border-faint pt-2 text-caption text-warning"
          >
            <DiamondShape />
            {full}
          </span>
        )}
      </span>
    </div>
  );
}

function ExpectedStatus({
  values,
  update,
  error,
}: {
  values: MonitorFormValues;
  update: <K extends keyof MonitorFormValues>(key: K, value: MonitorFormValues[K]) => void;
  error?: string;
}) {
  const exact = values.expectedMode === "exact";
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-caption font-medium">Expected status</span>
      <div className="flex flex-wrap items-start gap-2">
        <Segmented
          label="Expected status"
          name="expectedMode"
          value={values.expectedMode}
          onChange={(mode) => update("expectedMode", mode)}
          options={[
            { value: "2xx", label: "Any 2xx" },
            { value: "exact", label: "Exact code" },
          ]}
        />
        {exact && (
          <Field
            label={<span className="sr-only">Status code</span>}
            error={error}
            className="w-36"
          >
            {(c) => (
              <Input
                {...c}
                data-field="expectedStatus"
                name="expectedCode"
                mono
                inputMode="numeric"
                maxLength={3}
                placeholder="200"
                value={values.expectedCode}
                onChange={(e) => update("expectedCode", e.target.value)}
              />
            )}
          </Field>
        )}
      </div>
      <span className="text-caption text-muted-foreground">
        {exact
          ? "Only this code counts as up: 200 to 299, or 400 to 599. Anything else is a failure."
          : "Any answer from 200 to 299 counts as up."}
      </span>
    </div>
  );
}

function HeartbeatFields({
  values,
  update,
  errors,
}: {
  values: MonitorFormValues;
  update: <K extends keyof MonitorFormValues>(key: K, value: MonitorFormValues[K]) => void;
  errors: MonitorFormErrors;
}) {
  const human = (raw: string) =>
    /^[0-9]+$/.test(raw.trim()) ? ` = ${formatDuration(Number(raw))}.` : "";
  return (
    <>
      <Field
        label="Expected interval (seconds)"
        hint={`How often the sender pings: ${INTERVAL_MIN} to ${INTERVAL_MAX} seconds${human(values.expectedIntervalSeconds)} Set Alertmanager's repeat interval below it.`}
        error={errors.expectedIntervalSeconds}
      >
        {(c) => (
          <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
            <Input
              {...c}
              data-field="expectedIntervalSeconds"
              name="expectedIntervalSeconds"
              type="number"
              inputMode="numeric"
              min={INTERVAL_MIN}
              max={INTERVAL_MAX}
              mono
              end="s"
              className="sm:w-40"
              value={values.expectedIntervalSeconds}
              onChange={(e) => update("expectedIntervalSeconds", e.target.value)}
            />
            <div role="group" aria-label="Interval presets" className="flex flex-wrap gap-1.5">
              {INTERVAL_PRESETS.map((p) => {
                const on = values.expectedIntervalSeconds === String(p.seconds);
                return (
                  <Button
                    key={p.seconds}
                    variant={on ? "primary" : "secondary"}
                    aria-pressed={on}
                    mono
                    onClick={() => update("expectedIntervalSeconds", String(p.seconds))}
                  >
                    {p.label}
                  </Button>
                );
              })}
            </div>
          </div>
        )}
      </Field>

      <Field
        label="Grace (seconds)"
        hint={`Extra time after the interval before the watchdog is down: 0 to ${GRACE_MAX} seconds${human(values.graceSeconds)}`}
        error={errors.graceSeconds}
      >
        {(c) => (
          <Input
            {...c}
            data-field="graceSeconds"
            name="graceSeconds"
            type="number"
            inputMode="numeric"
            min={0}
            max={GRACE_MAX}
            mono
            end="s"
            className="sm:w-40"
            value={values.graceSeconds}
            onChange={(e) => update("graceSeconds", e.target.value)}
          />
        )}
      </Field>
    </>
  );
}
