import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
  resumeMonitor,
  type MonitorDetail,
  type StatsRange,
} from "../api/endpoints";
import ResponseChart from "../components/ResponseChart";
import { ErrorState, Loading } from "../components/QueryState";
import { useToast } from "../components/Toasts";
import {
  daysUntil,
  describeErrorKind,
  formatClock,
  formatDateTime,
  formatDuration,
  formatMs,
  formatPercent,
  secondsBetween,
} from "../lib/format";
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
  if (monitor.isError && !monitor.data) {
    if (monitor.error instanceof ApiError && monitor.error.status === 404) {
      return (
        <section>
          <h1>Monitor not found</h1>
          <p>It may have been deleted.</p>
          <Link to="/">Go to monitors</Link>
        </section>
      );
    }
    return (
      <ErrorState
        title="Cannot load the monitor"
        error={monitor.error}
        onRetry={() => void monitor.refetch()}
      />
    );
  }
  return <Details monitor={monitor.data} updatedAt={monitor.dataUpdatedAt} />;
}

function Details({ monitor, updatedAt }: { monitor: MonitorDetail; updatedAt: number }) {
  const now = useNow();
  const [range, setRange] = useState<StatsRange>("24h");
  const stats = useQuery({
    queryKey: queryKeys.stats(monitor.id, range),
    queryFn: () => getMonitorStats(monitor.id, range),
    refetchInterval: POLL_MS,
  });
  const checks = useQuery({
    queryKey: queryKeys.checks(monitor.id),
    queryFn: () => listChecks(monitor.id),
    refetchInterval: POLL_MS,
  });
  const incidents = useQuery({
    queryKey: queryKeys.incidents(monitor.id),
    queryFn: () => listIncidents(monitor.id),
    refetchInterval: POLL_MS,
  });

  const tls = monitor.tlsExpiresAt ? daysUntil(monitor.tlsExpiresAt, now) : null;

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
          value={
            tls == null ? "—" : tls < 0 ? "Expired" : `in ${tls} ${tls === 1 ? "day" : "days"}`
          }
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

      <p>
        <small>Last updated {formatClock(updatedAt)}</small>
      </p>
    </section>
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
          <p className="url">{monitor.url}</p>
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
          <button type="button" className="secondary outline" onClick={() => setConfirming(true)}>
            Delete
          </button>
        </div>
      </div>
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
