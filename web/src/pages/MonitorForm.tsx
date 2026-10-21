import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ApiError, planLimitMessage } from "../api/client";
import {
  createMonitor,
  getMonitor,
  queryKeys,
  updateMonitor,
  type MonitorDetail,
} from "../api/endpoints";
import HeartbeatUrlPanel from "../components/HeartbeatUrlPanel";
import MonitorNotFound from "../components/MonitorNotFound";
import { ErrorState, Loading } from "../components/QueryState";
import { useToast } from "../components/Toasts";
import { GRACE_MAX, INTERVAL_MAX, INTERVAL_MIN, INTERVAL_PRESETS } from "../lib/heartbeat";
import {
  emptyMonitorForm,
  mapServerErrors,
  NAME_MAX,
  toCreateRequest,
  toMonitorRequest,
  URL_MAX,
  validateMonitorForm,
  valuesFromMonitor,
  type FormField,
  type MonitorFormErrors,
  type MonitorFormValues,
} from "../lib/monitorForm";
import { atHeartbeatLimit, atMonitorLimit, useUsage } from "../lib/usage";

export default function MonitorForm({ mode }: { mode: "create" | "edit" }) {
  const { id = "" } = useParams();
  const monitor = useQuery({
    queryKey: queryKeys.monitor(id),
    queryFn: () => getMonitor(id),
    enabled: mode === "edit",
  });

  if (mode === "create") {
    return (
      <section>
        <h1>Add monitor</h1>
        <MonitorFormBody mode="create" initial={emptyMonitorForm} />
      </section>
    );
  }
  return (
    <section>
      <h1>Edit monitor</h1>
      {monitor.isPending ? (
        <Loading label="Loading the monitor…" />
      ) : monitor.isError && monitor.error instanceof ApiError && monitor.error.status === 404 ? (
        <MonitorNotFound />
      ) : monitor.isError ? (
        <ErrorState
          title="Cannot load the monitor"
          error={monitor.error}
          onRetry={() => void monitor.refetch()}
        />
      ) : (
        <MonitorFormBody
          key={monitor.data.id}
          mode="edit"
          id={monitor.data.id}
          initial={valuesFromMonitor(monitor.data)}
        />
      )}
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

  const save = useMutation({
    mutationFn: (v: MonitorFormValues) =>
      mode === "create"
        ? createMonitor(toCreateRequest(v))
        : updateMonitor(id!, toMonitorRequest(v)),
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
    if (Object.keys(found).length > 0) return;
    save.mutate(values);
  }

  const invalid = (field: FormField) => (errors[field] ? true : undefined);
  const describedBy = (field: FormField, hint?: string) =>
    [errors[field] ? `${field}-error` : null, hint].filter(Boolean).join(" ") || undefined;
  const fieldError = (field: FormField) =>
    errors[field] ? <small id={`${field}-error`}>{errors[field]}</small> : null;
  const hasErrors = Object.values(errors).some(Boolean);
  const cancelTo = mode === "edit" && id ? `/monitors/${id}` : "/";
  const heartbeat = values.type === "heartbeat";
  const httpFull = atMonitorLimit(usage.data);
  const heartbeatFull = atHeartbeatLimit(usage.data);

  if (created?.heartbeatUrl) {
    return (
      <HeartbeatUrlPanel
        url={created.heartbeatUrl}
        expectedIntervalSeconds={created.expectedIntervalSeconds ?? created.intervalSeconds}
      >
        <Link to={`/monitors/${created.id}`} role="button">
          I saved the URL, open the monitor
        </Link>
      </HeartbeatUrlPanel>
    );
  }

  return (
    <form onSubmit={onSubmit} noValidate>
      {(general || hasErrors) && (
        <p role="alert" className="error">
          {general ?? "Check the highlighted fields."}
        </p>
      )}

      {mode === "create" && (
        <fieldset aria-describedby="type-hint">
          <legend>What to monitor</legend>
          <label>
            <input
              type="radio"
              name="type"
              checked={!heartbeat}
              onChange={() => update("type", "http")}
              aria-describedby={httpFull ? "http-full" : undefined}
            />
            HTTP check
          </label>
          {httpFull && (
            <small id="http-full" className="plan-full">
              Your plan&apos;s HTTP monitor limit is reached.
            </small>
          )}
          <label>
            <input
              type="radio"
              name="type"
              checked={heartbeat}
              onChange={() => update("type", "heartbeat")}
              aria-describedby={heartbeatFull ? "heartbeat-full" : undefined}
            />
            Watchdog (heartbeat)
          </label>
          {heartbeatFull && (
            <small id="heartbeat-full" className="plan-full">
              Your plan&apos;s watchdog limit is reached.
            </small>
          )}
          <small id="type-hint">
            An HTTP check calls your HTTPS endpoint every minute. A watchdog waits for pings, for
            example Alertmanager&apos;s always-firing Watchdog alert, and alerts when they stop.
          </small>
        </fieldset>
      )}

      <label htmlFor="name">Name</label>
      <input
        id="name"
        name="name"
        maxLength={NAME_MAX * 2}
        value={values.name}
        onChange={(e) => update("name", e.target.value)}
        aria-invalid={invalid("name")}
        aria-describedby={describedBy("name")}
      />
      {fieldError("name")}

      {heartbeat ? (
        <HeartbeatFields values={values} update={update} errors={errors} />
      ) : (
        <>
          <label htmlFor="url">URL</label>
          <input
            id="url"
            name="url"
            type="url"
            inputMode="url"
            maxLength={URL_MAX}
            placeholder="https://example.com/health"
            value={values.url}
            onChange={(e) => update("url", e.target.value)}
            aria-invalid={invalid("url")}
            aria-describedby={describedBy("url", "url-hint")}
          />
          {fieldError("url")}
          <small id="url-hint">HTTPS only. Private and internal addresses are not allowed.</small>

          <div className="grid">
            <label>
              Check every
              <input readOnly value="60 seconds" aria-describedby="fixed-hint" />
            </label>
            <label>
              Timeout
              <input readOnly value="10 seconds" aria-describedby="fixed-hint" />
            </label>
          </div>
          <small id="fixed-hint">The interval and timeout are fixed for now.</small>

          <fieldset aria-describedby={describedBy("expectedStatus")}>
            <legend>Expected status</legend>
            <label>
              <input
                type="radio"
                name="expectedMode"
                checked={values.expectedMode === "2xx"}
                onChange={() => update("expectedMode", "2xx")}
              />
              Any 2xx
            </label>
            <label>
              <input
                type="radio"
                name="expectedMode"
                checked={values.expectedMode === "exact"}
                onChange={() => update("expectedMode", "exact")}
              />
              Exact code
            </label>
            {values.expectedMode === "exact" && (
              <label>
                Status code
                <input
                  name="expectedCode"
                  inputMode="numeric"
                  maxLength={3}
                  value={values.expectedCode}
                  onChange={(e) => update("expectedCode", e.target.value)}
                  aria-invalid={invalid("expectedStatus")}
                  aria-describedby={describedBy("expectedStatus")}
                />
              </label>
            )}
            {fieldError("expectedStatus")}
          </fieldset>

          <label htmlFor="failureThreshold">Alert after</label>
          <select
            id="failureThreshold"
            name="failureThreshold"
            value={values.failureThreshold}
            onChange={(e) => update("failureThreshold", Number(e.target.value))}
            aria-invalid={invalid("failureThreshold")}
            aria-describedby={describedBy("failureThreshold")}
          >
            <option value={1}>1 failed check</option>
            <option value={2}>2 failed checks in a row</option>
            <option value={3}>3 failed checks in a row</option>
          </select>
          {fieldError("failureThreshold")}
        </>
      )}

      <fieldset>
        <label>
          <input
            type="checkbox"
            name="notifySlack"
            checked={values.notifySlack}
            onChange={(e) => update("notifySlack", e.target.checked)}
            aria-invalid={invalid("notifySlack")}
            aria-describedby={describedBy("notifySlack", "notify-hint")}
          />
          Notify Slack
        </label>
        {fieldError("notifySlack")}
        <small id="notify-hint">
          {heartbeat
            ? "Slack gets a message when no ping arrives in time and when pings resume."
            : "Slack also gets a warning when the TLS certificate expires in 14 days or less."}
        </small>
      </fieldset>

      <div className="actions">
        <button type="submit" aria-busy={save.isPending} disabled={save.isPending}>
          {mode === "create" ? "Create monitor" : "Save changes"}
        </button>
        <Link to={cancelTo} role="button" className="secondary">
          Cancel
        </Link>
      </div>
    </form>
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
  const describedBy = (field: FormField, hint: string) =>
    [errors[field] ? `${field}-error` : null, hint].filter(Boolean).join(" ");
  const fieldError = (field: FormField) =>
    errors[field] ? <small id={`${field}-error`}>{errors[field]}</small> : null;

  return (
    <>
      <label htmlFor="expectedIntervalSeconds">Expected interval (seconds)</label>
      <input
        id="expectedIntervalSeconds"
        name="expectedIntervalSeconds"
        type="number"
        inputMode="numeric"
        min={INTERVAL_MIN}
        max={INTERVAL_MAX}
        value={values.expectedIntervalSeconds}
        onChange={(e) => update("expectedIntervalSeconds", e.target.value)}
        aria-invalid={errors.expectedIntervalSeconds ? true : undefined}
        aria-describedby={describedBy("expectedIntervalSeconds", "interval-hint")}
      />
      {fieldError("expectedIntervalSeconds")}
      <div role="group" aria-label="Interval presets" className="actions">
        {INTERVAL_PRESETS.map((p) => (
          <button
            key={p.seconds}
            type="button"
            className={values.expectedIntervalSeconds === String(p.seconds) ? undefined : "outline"}
            aria-pressed={values.expectedIntervalSeconds === String(p.seconds)}
            onClick={() => update("expectedIntervalSeconds", String(p.seconds))}
          >
            {p.label}
          </button>
        ))}
      </div>
      <small id="interval-hint">
        How often the sender pings: {INTERVAL_MIN} to {INTERVAL_MAX} seconds. Set
        Alertmanager&apos;s repeat interval below it.
      </small>

      <label htmlFor="graceSeconds">Grace (seconds)</label>
      <input
        id="graceSeconds"
        name="graceSeconds"
        type="number"
        inputMode="numeric"
        min={0}
        max={GRACE_MAX}
        value={values.graceSeconds}
        onChange={(e) => update("graceSeconds", e.target.value)}
        aria-invalid={errors.graceSeconds ? true : undefined}
        aria-describedby={describedBy("graceSeconds", "grace-hint")}
      />
      {fieldError("graceSeconds")}
      <small id="grace-hint">
        Extra time after the interval before the watchdog is down: 0 to {GRACE_MAX} seconds.
      </small>
    </>
  );
}
