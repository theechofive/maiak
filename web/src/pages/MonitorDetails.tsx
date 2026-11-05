import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ApiError, errorMessage, errorMeta } from "../api/client";
import {
  deleteMonitor,
  getMonitor,
  getMonitorStats,
  listChecks,
  listIncidents,
  pauseMonitor,
  POLL_MS,
  queryKeys,
  regenerateHeartbeatToken,
  resumeMonitor,
  type Check,
  type Incident,
  type MonitorDetail,
  type StatsRange,
} from "../api/endpoints";
import HeartbeatUrlPanel from "../components/HeartbeatUrlPanel";
import MonitorNotFound from "../components/MonitorNotFound";
import { useToast } from "../components/Toasts";
import {
  Alert,
  Badge,
  Button,
  buttonClass,
  ConfirmDialog,
  cx,
  ErrorState,
  Metric,
  MetricStrip,
  RibbonPlaceholder,
  RibbonSkeleton,
  Segmented,
  SignalRibbon,
  Skeleton,
  StatusIndicator,
  Table,
  Td,
  Th,
  Tr,
  WriteOnlyValue,
  Loading,
  EmptyNote,
} from "../components/ui";
import {
  daysUntil,
  describeErrorKind,
  formatAgo,
  formatClock,
  formatDate,
  formatDateTime,
  formatDuration,
  formatMs,
  formatPercent,
  formatSince,
  formatTime,
  formatTlsExpiry,
  secondsBetween,
} from "../lib/format";
import {
  expectationText,
  heartbeatDeadline,
  heartbeatStatusLine,
  isHeartbeat,
} from "../lib/heartbeat";
import {
  checkSettings,
  downReason,
  failureStreak,
  monitorState,
  statusLine,
  statusText,
} from "../lib/monitors";
import { axisLabels, incidentSpans, seriesSpan } from "../lib/ribbon";
import { useNow } from "../lib/useNow";
import { useAuth } from "../auth/AuthProvider";
import { abilities, ROLE_HINTS } from "../lib/roles";

const RANGES: { value: StatsRange; label: string; name: string; bucket: string }[] = [
  { value: "24h", label: "24h", name: "24 hours", bucket: "15 min" },
  { value: "7d", label: "7d", name: "7 days", bucket: "hour" },
  { value: "30d", label: "30d", name: "30 days", bucket: "6 h" },
];

/** The TLS certificate is worth a warning this many days before it expires. */
const TLS_WARN_DAYS = 14;

export default function MonitorDetails() {
  const { id = "" } = useParams();
  const monitor = useQuery({
    queryKey: queryKeys.monitor(id),
    queryFn: () => getMonitor(id),
    refetchInterval: POLL_MS,
  });

  if (monitor.isPending) {
    return (
      <Loading label="Loading the monitor…" className="pt-10">
        <Skeleton className="w-24" />
        <Skeleton className="h-8 w-72" />
        <Skeleton className="w-96 max-w-full" />
      </Loading>
    );
  }
  if (monitor.isError) {
    if (monitor.error instanceof ApiError && monitor.error.status === 404) {
      return <MonitorNotFound />;
    }
    if (!monitor.data) {
      return (
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
  }
  return (
    <Details
      monitor={monitor.data}
      updatedAt={monitor.dataUpdatedAt}
      updateFailed={monitor.isError}
    />
  );
}

function Details({
  monitor,
  updatedAt,
  updateFailed,
}: {
  monitor: MonitorDetail;
  updatedAt: number;
  updateFailed: boolean;
}) {
  const now = useNow();
  const heartbeat = isHeartbeat(monitor);
  const checks = useQuery({
    queryKey: queryKeys.checks(monitor.id),
    queryFn: () => listChecks(monitor.id),
    refetchInterval: POLL_MS,
    enabled: !heartbeat,
  });
  const incidents = useQuery({
    queryKey: queryKeys.incidents(monitor.id),
    queryFn: () => listIncidents(monitor.id),
    refetchInterval: POLL_MS,
  });
  const state = monitorState(monitor);

  return (
    <section className="flex flex-col gap-8 pt-4">
      <Header monitor={monitor} latestCheck={checks.data?.items[0]} now={now} />
      {state === "down" && <DownBrief monitor={monitor} checks={checks.data?.items} now={now} />}
      <TlsWarning monitor={monitor} now={now} />

      {heartbeat ? (
        <HeartbeatSection monitor={monitor} now={now} />
      ) : (
        <>
          <ResponseTime monitor={monitor} incidents={incidents.data?.items ?? []} now={now} />
          <MetricStrip>
            <Metric
              label="Uptime 24h"
              value={formatPercent(monitor.uptime.h24)}
              tone={state === "down" ? "down" : "default"}
            />
            <Metric label="Uptime 7d" value={formatPercent(monitor.uptime.d7)} />
            <Metric label="Uptime 30d" value={formatPercent(monitor.uptime.d30)} />
            <TlsMetric monitor={monitor} now={now} />
          </MetricStrip>
        </>
      )}

      <Section title="Incidents" note={incidents.data && incidentCount(incidents.data.items)}>
        {incidents.isPending ? (
          <TableSkeleton label="Loading incidents…" />
        ) : incidents.isError && !incidents.data ? (
          <ErrorState
            variant="section"
            title="Cannot load incidents"
            onRetry={() => void incidents.refetch()}
          />
        ) : incidents.data.items.length === 0 ? (
          <EmptyNote>No incidents.</EmptyNote>
        ) : (
          <IncidentTable items={incidents.data.items} now={now} />
        )}
      </Section>

      {!heartbeat && (
        <Section title="Recent checks" note={checks.data && `Latest ${checks.data.items.length}`}>
          {checks.isPending ? (
            <TableSkeleton label="Loading checks…" />
          ) : checks.isError && !checks.data ? (
            <ErrorState
              variant="section"
              title="Cannot load checks"
              onRetry={() => void checks.refetch()}
            />
          ) : checks.data.items.length === 0 ? (
            <EmptyNote>No checks yet. Results appear here as they come in.</EmptyNote>
          ) : (
            <CheckTable items={checks.data.items} />
          )}
        </Section>
      )}

      <p className="font-mono text-mono text-muted-foreground">
        Last updated {formatClock(updatedAt)}
        {updateFailed && (
          <>
            {" · "}
            <span role="status" className="text-down">
              Last update failed
            </span>
          </>
        )}
      </p>
    </section>
  );
}

function Header({
  monitor,
  latestCheck,
  now,
}: {
  monitor: MonitorDetail;
  latestCheck: Check | undefined;
  now: number;
}) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const toast = useToast();
  const canEdit = abilities(useAuth().session).editMonitors;
  const [confirming, setConfirming] = useState(false);
  const heartbeat = isHeartbeat(monitor);

  const toggle = useMutation({
    mutationFn: () => (monitor.paused ? resumeMonitor(monitor.id) : pauseMonitor(monitor.id)),
    onSuccess: (updated) => {
      queryClient.setQueryData(queryKeys.monitor(monitor.id), updated);
      void queryClient.invalidateQueries({ queryKey: queryKeys.monitors, exact: true });
      toast(updated.paused ? "Monitor paused" : "Monitor resumed");
    },
    onError: (err) => toast(errorMessage(err), "error"),
  });

  const remove = useMutation({
    mutationFn: () => deleteMonitor(monitor.id),
    onSuccess: () => {
      queryClient.removeQueries({ queryKey: queryKeys.monitor(monitor.id) });
      void queryClient.invalidateQueries({ queryKey: queryKeys.monitors, exact: true });
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      toast("Monitor deleted");
      navigate("/");
    },
    onError: (err) => {
      setConfirming(false);
      toast(errorMessage(err), "error");
    },
  });

  return (
    <div className="flex flex-col gap-3">
      <Link
        to="/"
        className="self-start py-2.5 text-[13px] text-muted-foreground hover:text-foreground sm:py-0"
      >
        ← Monitors
      </Link>
      <div className="flex flex-wrap items-end justify-between gap-x-6 gap-y-4">
        <div className="flex min-w-0 flex-col gap-2.5">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <StatusIndicator status={monitorState(monitor)} label={statusText(monitor, now)} />
            <span className="text-caption text-muted-foreground">
              {heartbeat
                ? heartbeatStatusLine(monitor, now)
                : statusLine(monitor, latestCheck, now)}
            </span>
          </div>
          <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1.5">
            <h1 className="min-w-0 text-headline leading-[1.1] font-semibold tracking-[-0.02em] break-words sm:text-display">
              {monitor.name}
            </h1>
            <Badge>{heartbeat ? "Watchdog" : "HTTP"}</Badge>
          </div>
          <span className="font-mono text-[13px] break-all">
            {heartbeat ? `Watchdog · ${expectationText(monitor)}` : monitor.url}
          </span>
          {!heartbeat && (
            <span className="text-caption text-muted-foreground">{checkSettings(monitor)}</span>
          )}
        </div>
        {canEdit ? (
          <div className="flex flex-wrap gap-2">
            <Link to={`/monitors/${monitor.id}/edit`} className={buttonClass("secondary")}>
              Edit
            </Link>
            <Button variant="secondary" busy={toggle.isPending} onClick={() => toggle.mutate()}>
              {monitor.paused ? "Resume" : "Pause"}
            </Button>
            <Button variant="ghost-destructive" onClick={() => setConfirming(true)}>
              Delete
            </Button>
          </div>
        ) : (
          <p className="max-w-[280px] text-caption text-muted-foreground">
            {ROLE_HINTS.editMonitors}
          </p>
        )}
      </div>
      {confirming && (
        <ConfirmDialog
          title={`Delete ${monitor.name}?`}
          confirmLabel="Delete monitor"
          busy={remove.isPending}
          onConfirm={() => remove.mutate()}
          onCancel={() => setConfirming(false)}
        >
          <p>
            Checks stop and the monitor leaves the list. Its history is kept until the retention
            period ends.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}

/** What is failing and since when, from the monitor and its latest checks. */
function DownBrief({
  monitor,
  checks,
  now,
}: {
  monitor: MonitorDetail;
  checks: Check[] | undefined;
  now: number;
}) {
  const heartbeat = isHeartbeat(monitor);
  const latest = checks?.[0];
  const streak = checks && failureStreak(checks);
  const since = `${formatSince(monitor.statusSince, now)} · ${formatDuration(secondsBetween(monitor.statusSince, now))}`;

  let failingDetail: string | undefined;
  if (heartbeat) failingDetail = `Last ping ${formatAgo(monitor.lastPingAt, now)}.`;
  else if (latest && !latest.ok) {
    // The kind is already the headline; the HTTP code is the new information.
    failingDetail =
      latest.httpStatus != null
        ? `Last check at ${formatTime(latest.checkedAt)}: HTTP ${latest.httpStatus}.`
        : `Last check at ${formatTime(latest.checkedAt)}.`;
  }

  let sinceDetail: string | undefined;
  if (heartbeat) sinceDetail = `${expectationText(monitor)}.`;
  else if (streak && streak.count > 0) {
    const count = `${streak.count}${streak.all ? "+" : ""}`;
    sinceDetail = `${count} failed ${streak.count === 1 ? "check" : "checks"} in a row.`;
    if (streak.lastSuccess)
      sinceDetail += ` Last success at ${formatTime(streak.lastSuccess.checkedAt)}.`;
  }

  return (
    <section
      aria-label="Ongoing incident"
      className="grid gap-0 border border-down-border bg-down-tint sm:grid-cols-2"
    >
      <BriefCell label="What's failing" value={heartbeat ? "No ping in time" : downReason(monitor)}>
        {failingDetail}
      </BriefCell>
      <BriefCell label="Since" value={since} mono>
        {sinceDetail}
      </BriefCell>
    </section>
  );
}

function BriefCell({
  label,
  value,
  mono = false,
  children,
}: {
  label: string;
  value: string;
  mono?: boolean;
  children?: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1.5 border-down-border px-5 py-4 not-first:max-sm:border-t sm:not-first:border-l">
      <span className="eyebrow text-down">{label}</span>
      <span className={cx("text-[17px] font-semibold", mono && "font-mono tracking-[-0.01em]")}>
        {value}
      </span>
      {children && <span className="text-caption">{children}</span>}
    </div>
  );
}

function TlsWarning({ monitor, now }: { monitor: MonitorDetail; now: number }) {
  if (!monitor.tlsExpiresAt || isHeartbeat(monitor)) return null;
  const days = daysUntil(monitor.tlsExpiresAt, now);
  if (days > TLS_WARN_DAYS) return null;
  const when = formatTlsExpiry(monitor.tlsExpiresAt, now);
  return days < 0 ? (
    <Alert title="The TLS certificate has expired.">
      Renew it: browsers refuse the site until then.
    </Alert>
  ) : (
    <Alert
      tone="warning"
      title={`The TLS certificate expires ${when}, on ${formatDate(monitor.tlsExpiresAt)}.`}
    >
      Renew it before then, or every check will start failing.
    </Alert>
  );
}

function TlsMetric({ monitor, now }: { monitor: MonitorDetail; now: number }) {
  if (!monitor.tlsExpiresAt) return <Metric label="TLS expires" value="—" />;
  const soon = daysUntil(monitor.tlsExpiresAt, now) <= TLS_WARN_DAYS;
  return (
    <Metric
      label="TLS expires"
      value={formatDate(monitor.tlsExpiresAt)}
      note={formatTlsExpiry(monitor.tlsExpiresAt, now)}
      tone={soon ? "warning" : "default"}
    />
  );
}

function ResponseTime({
  monitor,
  incidents,
  now,
}: {
  monitor: MonitorDetail;
  incidents: Incident[];
  now: number;
}) {
  const [range, setRange] = useState<StatsRange>("24h");
  const stats = useQuery({
    queryKey: queryKeys.stats(monitor.id, range),
    queryFn: () => getMonitorStats(monitor.id, range),
    placeholderData: keepPreviousData,
    refetchInterval: POLL_MS,
  });
  // Labels describe the data on screen: while another range loads, the previous one stays shown.
  const shown = stats.data?.range ?? range;
  const current = RANGES.find((r) => r.value === shown)!;
  const switching = stats.isPlaceholderData;
  const series = stats.data?.series ?? [];
  const span = stats.data && seriesSpan(series, stats.data.range);
  const ranAny = series.some((p) => p.okRatio != null);
  const spans = span ? incidentSpans(incidents, span, now) : [];

  return (
    <section aria-labelledby="response-time" className="flex flex-col gap-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <h2 id="response-time" className="eyebrow text-foreground">
            Response time
          </h2>
          <span className="text-caption text-muted-foreground">
            average per {current.bucket} bucket
          </span>
        </div>
        <Segmented
          label="Time range"
          name="range"
          size="small"
          value={range}
          onChange={setRange}
          options={RANGES.map((r) => ({ value: r.value, label: r.label }))}
        />
      </div>

      {stats.isPending ? (
        <Loading label="Loading response times…" className="pt-14">
          <RibbonSkeleton />
        </Loading>
      ) : stats.isError && !stats.data ? (
        <ErrorState
          variant="section"
          title="Cannot load response times"
          onRetry={() => void stats.refetch()}
        />
      ) : !ranAny ? (
        <RibbonPlaceholder>
          {monitor.paused
            ? "Paused: no checks ran in this range."
            : monitorState(monitor) === "pending"
              ? "No checks yet. The first one draws here."
              : "No checks ran in this range."}
        </RibbonPlaceholder>
      ) : (
        <>
          <div
            aria-busy={switching || undefined}
            className={cx("transition-opacity", switching && "opacity-50")}
          >
            <SignalRibbon
              points={series}
              label={`Response time, last ${current.name}`}
              heightPx={140}
              axis={span ? axisLabels(span, stats.data.range) : undefined}
              below={<IncidentLane spans={spans} incidents={incidents} />}
            />
          </div>
          <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-caption text-muted-foreground">
            <span className="font-mono text-foreground">
              Average {formatMs(stats.data.avgMs)} · p95 {formatMs(stats.data.p95Ms)}
            </span>
            <span className="flex items-center gap-1.5">
              <span className="h-2 w-1 bg-ribbon" aria-hidden="true" />
              Height: response time
            </span>
            <span className="flex items-center gap-1.5">
              <span className="h-2 w-1 bg-destructive" aria-hidden="true" />
              Red: failed checks
            </span>
            <span>Break: no checks ran</span>
            {spans.length > 0 && (
              <span className="flex items-center gap-1.5">
                <span className="h-1 w-3 bg-destructive" aria-hidden="true" />
                {incidentCount(incidents.filter((i) => spans.some((s) => s.id === i.id)))} in this
                range
              </span>
            )}
          </p>
        </>
      )}
    </section>
  );
}

/** Incidents on the ribbon's time axis, so a dip and its incident read as one story. */
function IncidentLane({
  spans,
  incidents,
}: {
  spans: ReturnType<typeof incidentSpans>;
  incidents: Incident[];
}) {
  if (spans.length === 0) return null;
  return (
    <div aria-hidden="true" className="relative mt-1.5 h-1.5">
      {spans.map((s) => {
        const incident = incidents.find((i) => i.id === s.id);
        return (
          <span
            key={s.id}
            title={
              incident &&
              `${formatDateTime(incident.startedAt)} · ${describeErrorKind(incident.reason)}${s.ongoing ? " · ongoing" : ""}`
            }
            className={cx("absolute inset-y-0", s.ongoing ? "bg-destructive" : "bg-down-border")}
            style={{ left: `${s.from * 100}%`, width: `max(3px, ${(s.to - s.from) * 100}%)` }}
          />
        );
      })}
    </div>
  );
}

function HeartbeatSection({ monitor, now }: { monitor: MonitorDetail; now: number }) {
  const toast = useToast();
  const canEdit = abilities(useAuth().session).editMonitors;
  const [regenerating, setRegenerating] = useState(false);
  const [newUrl, setNewUrl] = useState<string | undefined>();
  const deadline = heartbeatDeadline(monitor);

  const regenerate = useMutation({
    mutationFn: () => regenerateHeartbeatToken(monitor.id),
    onSuccess: (res) => {
      setRegenerating(false);
      setNewUrl(res.heartbeatUrl);
      toast("New URL created; the old one no longer works");
    },
    onError: (err) => {
      setRegenerating(false);
      toast(errorMessage(err), "error");
    },
  });

  return (
    <section aria-labelledby="heartbeat" className="flex flex-col gap-5">
      <div className="flex flex-col gap-1">
        <h2 id="heartbeat" className="text-lead font-semibold">
          Heartbeat
        </h2>
        <p className="text-muted-foreground">
          Your job calls the heartbeat URL each time it runs. Maiak alerts when a call is late.
        </p>
      </div>
      <MetricStrip>
        <Metric label="Last ping" value={formatAgo(monitor.lastPingAt, now)} />
        <Metric
          label="Expected every"
          value={formatDuration(monitor.expectedIntervalSeconds ?? monitor.intervalSeconds)}
        />
        <Metric label="Grace" value={formatDuration(monitor.graceSeconds ?? 0)} />
        <Metric
          label="Alert if silent by"
          value={deadline ? formatTime(deadline) : "—"}
          note={deadline && deadline < now ? "passed" : undefined}
        />
      </MetricStrip>
      {newUrl ? (
        <HeartbeatUrlPanel
          url={newUrl}
          expectedIntervalSeconds={monitor.expectedIntervalSeconds ?? monitor.intervalSeconds}
        >
          <Button variant="secondary" onClick={() => setNewUrl(undefined)}>
            I saved the URL, hide it
          </Button>
        </HeartbeatUrlPanel>
      ) : canEdit ? (
        <div className="max-w-[560px]">
          <WriteOnlyValue
            label="Heartbeat URL"
            hint="Shown once when it was created. Lost it? Regenerate it."
            actionLabel="Regenerate URL"
            onReplace={() => setRegenerating(true)}
          />
        </div>
      ) : (
        <p className="text-caption text-muted-foreground">
          The heartbeat URL is secret and shown only to whoever creates or regenerates it.
        </p>
      )}
      {regenerating && (
        <ConfirmDialog
          title={`Regenerate the URL of ${monitor.name}?`}
          confirmLabel="Regenerate URL"
          confirmVariant="primary"
          busy={regenerate.isPending}
          onConfirm={() => regenerate.mutate()}
          onCancel={() => setRegenerating(false)}
        >
          <p>
            The current URL stops working immediately. Update every sender (for example the file
            Alertmanager&apos;s url_file points to) with the new URL before the next deadline, or
            the watchdog goes down.
          </p>
        </ConfirmDialog>
      )}
    </section>
  );
}

function IncidentTable({ items, now }: { items: Incident[]; now: number }) {
  return (
    <Table>
      <thead>
        <tr>
          <Th>Started</Th>
          <Th>Duration</Th>
          <Th>Reason</Th>
          <Th numeric>Status</Th>
        </tr>
      </thead>
      <tbody>
        {items.map((i) => (
          <Tr key={i.id} tone={i.resolvedAt ? "default" : "down"}>
            <Td className="font-mono text-mono whitespace-nowrap">{formatDateTime(i.startedAt)}</Td>
            <Td className="font-mono text-mono whitespace-nowrap">
              {formatDuration(secondsBetween(i.startedAt, i.resolvedAt ?? now))}
            </Td>
            <Td>{describeErrorKind(i.reason)}</Td>
            <Td numeric>
              {i.resolvedAt ? <Badge>Resolved</Badge> : <Badge tone="danger">Ongoing</Badge>}
            </Td>
          </Tr>
        ))}
      </tbody>
    </Table>
  );
}

function CheckTable({ items }: { items: Check[] }) {
  return (
    <Table>
      <thead>
        <tr>
          <Th>Time</Th>
          <Th>Result</Th>
          <Th numeric>HTTP</Th>
          <Th numeric>Response</Th>
          <Th className="max-sm:hidden">Error</Th>
        </tr>
      </thead>
      <tbody>
        {items.map((c) => (
          <Tr key={`${c.checkedAt}-${c.location}`}>
            <Td className="font-mono text-mono whitespace-nowrap">{formatDateTime(c.checkedAt)}</Td>
            <Td>
              {c.ok ? (
                <StatusIndicator status="up" label="OK" />
              ) : (
                <StatusIndicator status="down" inline label="Failed" />
              )}
            </Td>
            <Td numeric>{c.httpStatus ?? "—"}</Td>
            <Td numeric>{formatMs(c.responseMs)}</Td>
            <Td className="text-muted-foreground max-sm:hidden">
              {c.ok ? "" : describeErrorKind(c.errorKind)}
            </Td>
          </Tr>
        ))}
      </tbody>
    </Table>
  );
}

function Section({
  title,
  note,
  children,
}: {
  title: string;
  note?: ReactNode;
  children: ReactNode;
}) {
  const id = `section-${title.toLowerCase().replace(/\W+/g, "-")}`;
  return (
    <section aria-labelledby={id} className="flex flex-col gap-3">
      <div className="flex items-baseline gap-3">
        <h2 id={id} className="text-lead font-semibold">
          {title}
        </h2>
        {note && <span className="text-caption text-muted-foreground">{note}</span>}
      </div>
      {children}
    </section>
  );
}

function TableSkeleton({ label }: { label: string }) {
  return (
    <Loading label={label} className="gap-0 border-t border-border">
      {["55%", "40%", "62%"].map((width) => (
        <div key={width} className="border-b border-border-faint py-3">
          <span className="block" style={{ width }}>
            <Skeleton />
          </span>
        </div>
      ))}
    </Loading>
  );
}

function incidentCount(items: Incident[]): string {
  const ongoing = items.filter((i) => !i.resolvedAt).length;
  const total = `${items.length} ${items.length === 1 ? "incident" : "incidents"}`;
  return ongoing > 0 ? `${total}, ${ongoing} ongoing` : total;
}
