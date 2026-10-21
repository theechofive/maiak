import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ApiError, errorMessage } from "../api/client";
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
  type MonitorDetail,
  type StatsRange,
} from "../api/endpoints";
import HeartbeatUrlPanel from "../components/HeartbeatUrlPanel";
import ResponseChart from "../components/ResponseChart";
import MonitorNotFound from "../components/MonitorNotFound";
import { ErrorState, Loading } from "../components/QueryState";
import { useToast } from "../components/Toasts";
import {
  describeErrorKind,
  formatAgo,
  formatClock,
  formatDateTime,
  formatDuration,
  formatMs,
  formatPercent,
  formatTlsExpiry,
  secondsBetween,
} from "../lib/format";
import { expectationText, heartbeatStatusLine, isHeartbeat } from "../lib/heartbeat";
import { statusLine } from "../lib/monitors";
import { useNow } from "../lib/useNow";

const ranges: { value: StatsRange; label: string }[] = [
  { value: "24h", label: "24 h" },
  { value: "7d", label: "7 d" },
  { value: "30d", label: "30 d" },
];

export default function MonitorDetails() {
  const { id = "" } = useParams();
  const monitor = useQuery({
    queryKey: queryKeys.monitor(id),
    queryFn: () => getMonitor(id),
    refetchInterval: POLL_MS,
  });

  if (monitor.isPending) return <Loading label="Loading the monitor…" />;
  if (monitor.isError) {
    if (monitor.error instanceof ApiError && monitor.error.status === 404) {
      return <MonitorNotFound />;
    }
    if (!monitor.data) {
      return (
        <ErrorState
          title="Cannot load the monitor"
          error={monitor.error}
          onRetry={() => void monitor.refetch()}
        />
      );
    }
  }
  const Shown = isHeartbeat(monitor.data) ? HeartbeatDetails : Details;
  return (
    <Shown
      monitor={monitor.data}
      updatedAt={monitor.dataUpdatedAt}
      updateFailed={monitor.isError}
    />
  );
}

/** A watchdog: last ping and expectations, incidents; no response times or checks. */
function HeartbeatDetails({
  monitor,
  updatedAt,
  updateFailed,
}: {
  monitor: MonitorDetail;
  updatedAt: number;
  updateFailed: boolean;
}) {
  const now = useNow();
  return (
    <section>
      <Header monitor={monitor} />
      <p className={`status status-${monitor.paused ? "paused" : monitor.status}`}>
        {heartbeatStatusLine(monitor, now)}
      </p>
      <div className="grid metrics">
        <Metric label="Last ping" value={formatAgo(monitor.lastPingAt, now)} />
        <Metric
          label="Expected interval"
          value={formatDuration(monitor.expectedIntervalSeconds ?? monitor.intervalSeconds)}
        />
        <Metric label="Grace" value={formatDuration(monitor.graceSeconds ?? 0)} />
      </div>
      <Incidents monitorId={monitor.id} now={now} />
      <UpdatedNote updatedAt={updatedAt} updateFailed={updateFailed} />
    </section>
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
  const [range, setRange] = useState<StatsRange>("24h");
  const stats = useQuery({
    queryKey: queryKeys.stats(monitor.id, range),
    queryFn: () => getMonitorStats(monitor.id, range),
    placeholderData: keepPreviousData,
    refetchInterval: POLL_MS,
  });
  const checks = useQuery({
    queryKey: queryKeys.checks(monitor.id),
    queryFn: () => listChecks(monitor.id),
    refetchInterval: POLL_MS,
  });
  return (
    <section>
      <Header monitor={monitor} />
      <p className={`status status-${monitor.paused ? "paused" : monitor.status}`}>
        {statusLine(monitor, checks.data?.items[0], now)}
      </p>

      <div className="grid metrics">
        <Metric label="Uptime 24h" value={formatPercent(monitor.uptime.h24)} />
        <Metric label="Uptime 7d" value={formatPercent(monitor.uptime.d7)} />
        <Metric label="Uptime 30d" value={formatPercent(monitor.uptime.d30)} />
        <Metric
          label="TLS expires"
          value={monitor.tlsExpiresAt ? formatTlsExpiry(monitor.tlsExpiresAt, now) : "—"}
        />
      </div>

      <section aria-labelledby="chart-heading">
        <div className="page-heading">
          <h2 id="chart-heading">Response time</h2>
          <div role="group" aria-label="Time range">
            {ranges.map((r) => (
              <button
                key={r.value}
                type="button"
                className={r.value === range ? undefined : "outline"}
                aria-pressed={r.value === range}
                onClick={() => setRange(r.value)}
              >
                {r.label}
              </button>
            ))}
          </div>
        </div>
        {stats.isPending ? (
          <Loading label="Loading response times…" />
        ) : stats.isError && !stats.data ? (
          <ErrorState
            title="Cannot load response times"
            error={stats.error}
            onRetry={() => void stats.refetch()}
          />
        ) : (
          <>
            <ResponseChart stats={stats.data} />
            <p>
              <small>
                Average {formatMs(stats.data.avgMs)} · p95 {formatMs(stats.data.p95Ms)}
              </small>
            </p>
          </>
        )}
      </section>

      <Incidents monitorId={monitor.id} now={now} />

      <section aria-labelledby="checks-heading">
        <h2 id="checks-heading">Recent checks</h2>
        {checks.isPending ? (
          <Loading label="Loading checks…" />
        ) : checks.isError && !checks.data ? (
          <ErrorState
            title="Cannot load checks"
            error={checks.error}
            onRetry={() => void checks.refetch()}
          />
        ) : checks.data.items.length === 0 ? (
          <p>No checks yet.</p>
        ) : (
          <div className="overflow-auto">
            <table className="striped">
              <thead>
                <tr>
                  <th scope="col">Time</th>
                  <th scope="col">Result</th>
                  <th scope="col">Code</th>
                  <th scope="col">Response</th>
                </tr>
              </thead>
              <tbody>
                {checks.data.items.map((c) => (
                  <tr key={`${c.checkedAt}-${c.location}`}>
                    <td>{formatDateTime(c.checkedAt)}</td>
                    <td className={c.ok ? "status-up" : "status-down"}>
                      {c.ok ? "OK" : `Failed: ${describeErrorKind(c.errorKind)}`}
                    </td>
                    <td>{c.httpStatus ?? "—"}</td>
                    <td>{formatMs(c.responseMs)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <UpdatedNote updatedAt={updatedAt} updateFailed={updateFailed} />
    </section>
  );
}

function Incidents({ monitorId, now }: { monitorId: string; now: number }) {
  const incidents = useQuery({
    queryKey: queryKeys.incidents(monitorId),
    queryFn: () => listIncidents(monitorId),
    refetchInterval: POLL_MS,
  });
  return (
    <section aria-labelledby="incidents-heading">
      <h2 id="incidents-heading">Incidents</h2>
      {incidents.isPending ? (
        <Loading label="Loading incidents…" />
      ) : incidents.isError && !incidents.data ? (
        <ErrorState
          title="Cannot load incidents"
          error={incidents.error}
          onRetry={() => void incidents.refetch()}
        />
      ) : incidents.data.items.length === 0 ? (
        <p>No incidents.</p>
      ) : (
        <div className="overflow-auto">
          <table>
            <thead>
              <tr>
                <th scope="col">Started</th>
                <th scope="col">Duration</th>
                <th scope="col">Reason</th>
              </tr>
            </thead>
            <tbody>
              {incidents.data.items.map((i) => (
                <tr key={i.id}>
                  <td>{formatDateTime(i.startedAt)}</td>
                  <td>
                    {i.resolvedAt
                      ? formatDuration(secondsBetween(i.startedAt, i.resolvedAt))
                      : `Ongoing, ${formatDuration(secondsBetween(i.startedAt, now))}`}
                  </td>
                  <td>{describeErrorKind(i.reason)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function UpdatedNote({ updatedAt, updateFailed }: { updatedAt: number; updateFailed: boolean }) {
  return (
    <p>
      <small>
        Last updated {formatClock(updatedAt)}
        {updateFailed && (
          <>
            {" · "}
            <span role="status" className="error">
              Last update failed
            </span>
          </>
        )}
      </small>
    </p>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <article className="metric">
      <small>{label}</small>
      <p>
        <strong>{value}</strong>
      </p>
    </article>
  );
}

function Header({ monitor }: { monitor: MonitorDetail }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const toast = useToast();
  const [confirming, setConfirming] = useState(false);
  const [regenerating, setRegenerating] = useState(false);
  const [newUrl, setNewUrl] = useState<string | undefined>();

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
    <>
      <div className="page-heading">
        <hgroup>
          <h1>{monitor.name}</h1>
          <p className="url">
            {isHeartbeat(monitor) ? `Watchdog · ${expectationText(monitor)}` : monitor.url}
          </p>
        </hgroup>
        <div className="actions">
          <Link to={`/monitors/${monitor.id}/edit`} role="button" className="secondary">
            Edit
          </Link>
          <button
            type="button"
            className="secondary"
            aria-busy={toggle.isPending}
            disabled={toggle.isPending}
            onClick={() => toggle.mutate()}
          >
            {monitor.paused ? "Resume" : "Pause"}
          </button>
          {isHeartbeat(monitor) && (
            <button type="button" className="secondary" onClick={() => setRegenerating(true)}>
              Regenerate URL
            </button>
          )}
          <button type="button" className="secondary outline" onClick={() => setConfirming(true)}>
            Delete
          </button>
        </div>
      </div>
      {newUrl && (
        <HeartbeatUrlPanel
          url={newUrl}
          expectedIntervalSeconds={monitor.expectedIntervalSeconds ?? monitor.intervalSeconds}
        >
          <button type="button" className="secondary" onClick={() => setNewUrl(undefined)}>
            I saved the URL, hide it
          </button>
        </HeartbeatUrlPanel>
      )}
      {regenerating && (
        <dialog
          open
          aria-labelledby="regenerate-title"
          aria-describedby="regenerate-desc"
          onKeyDown={(e) => {
            if (e.key === "Escape") setRegenerating(false);
          }}
        >
          <article>
            <h2 id="regenerate-title">Regenerate the URL of {monitor.name}?</h2>
            <p id="regenerate-desc">
              The current URL stops working immediately. Update every sender (for example the file
              Alertmanager&apos;s url_file points to) with the new URL before the next deadline, or
              the watchdog goes down.
            </p>
            <footer className="actions">
              <button
                type="button"
                className="secondary"
                autoFocus
                onClick={() => setRegenerating(false)}
              >
                Cancel
              </button>
              <button
                type="button"
                aria-busy={regenerate.isPending}
                disabled={regenerate.isPending}
                onClick={() => regenerate.mutate()}
              >
                Regenerate URL
              </button>
            </footer>
          </article>
        </dialog>
      )}
      {confirming && (
        <dialog
          open
          aria-labelledby="delete-title"
          aria-describedby="delete-desc"
          onKeyDown={(e) => {
            if (e.key === "Escape") setConfirming(false);
          }}
        >
          <article>
            <h2 id="delete-title">Delete {monitor.name}?</h2>
            <p id="delete-desc">
              Checks stop and the monitor leaves the list. Its history is kept until the retention
              period ends.
            </p>
            <footer className="actions">
              <button
                type="button"
                className="secondary"
                autoFocus
                onClick={() => setConfirming(false)}
              >
                Cancel
              </button>
              <button
                type="button"
                aria-busy={remove.isPending}
                disabled={remove.isPending}
                onClick={() => remove.mutate()}
              >
                Delete monitor
              </button>
            </footer>
          </article>
        </dialog>
      )}
    </>
  );
}
