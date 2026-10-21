import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { listMonitors, POLL_MS, queryKeys, type Usage } from "../api/endpoints";
import { ErrorState, Loading } from "../components/QueryState";
import { formatAgo, formatClock, formatMs, formatPercent } from "../lib/format";
import { filterMonitors, statusText, summaryLine } from "../lib/monitors";
import { atMonitorLimit, countOf, useUsage } from "../lib/usage";
import { useNow } from "../lib/useNow";

export default function Monitors() {
  const navigate = useNavigate();
  const now = useNow();
  const [query, setQuery] = useState("");
  const monitors = useQuery({
    queryKey: queryKeys.monitors,
    queryFn: listMonitors,
    refetchInterval: POLL_MS,
  });
  const usage = useUsage();

  const heading = (
    <div className="page-heading">
      <h1>Monitors</h1>
      {monitors.data && monitors.data.items.length > 0 && <AddMonitor usage={usage.data} />}
    </div>
  );

  if (monitors.isPending) {
    return (
      <section>
        {heading}
        <Loading label="Loading monitors…" />
      </section>
    );
  }
  if (monitors.isError && !monitors.data) {
    return (
      <section>
        {heading}
        <ErrorState
          title="Cannot load monitors"
          error={monitors.error}
          onRetry={() => void monitors.refetch()}
        />
      </section>
    );
  }

  const list = monitors.data;
  if (list.items.length === 0) {
    return (
      <section>
        {heading}
        <article className="empty-state">
          <h2>Start your first monitor</h2>
          <p>
            Maiak checks your HTTPS endpoint every minute and alerts you in Slack when it fails.
          </p>
          {usage.data && !atMonitorLimit(usage.data) && (
            <p>
              Your {usage.data.plan.name} plan includes{" "}
              {countOf(usage.data.limits.monitors, "monitor", "monitors")}.
            </p>
          )}
          <AddMonitor usage={usage.data} />
        </article>
      </section>
    );
  }

  const visible = filterMonitors(list.items, query);

  return (
    <section>
      {heading}
      <p>{summaryLine(list)}</p>
      <input
        type="search"
        aria-label="Filter by name or URL"
        placeholder="Filter by name or URL"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
      />
      <div className="overflow-auto">
        <table className="striped">
          <thead>
            <tr>
              <th scope="col">Status</th>
              <th scope="col">Name</th>
              <th scope="col">URL</th>
              <th scope="col">Uptime 24h</th>
              <th scope="col">Response</th>
              <th scope="col">Checked</th>
            </tr>
          </thead>
          <tbody>
            {visible.map((m) => (
              <tr key={m.id} className="clickable" onClick={() => navigate(`/monitors/${m.id}`)}>
                <td>
                  <span className={`status status-${m.paused ? "paused" : m.status}`}>
                    {statusText(m, now)}
                  </span>
                </td>
                <td>
                  <Link to={`/monitors/${m.id}`} onClick={(e) => e.stopPropagation()}>
                    {m.name}
                  </Link>
                </td>
                <td className="url">{m.url}</td>
                <td>{formatPercent(m.uptime24h)}</td>
                <td>{formatMs(m.lastResponseMs)}</td>
                <td>{formatAgo(m.lastCheckedAt, now)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {visible.length === 0 && <p>No monitors match “{query.trim()}”.</p>}
      <p>
        <small>
          Last updated {formatClock(monitors.dataUpdatedAt)}
          {monitors.isError && " · The last update failed, retrying"}
        </small>
      </p>
    </section>
  );
}

/**
 * The add button, or at the plan's monitor limit a button marked aria-disabled with the reason
 * next to it (the server enforces the limit either way).
 */
function AddMonitor({ usage }: { usage: Usage | undefined }) {
  if (!usage || !atMonitorLimit(usage)) {
    return (
      <Link to="/monitors/new" role="button">
        Add monitor
      </Link>
    );
  }
  return (
    <div className="plan-limit">
      <button
        type="button"
        className="secondary"
        aria-disabled="true"
        aria-describedby="monitor-limit"
      >
        Add monitor
      </button>
      <small id="monitor-limit">
        The {usage.plan.name} plan allows {countOf(usage.limits.monitors, "monitor", "monitors")}.
        Delete a monitor to add a new one.
      </small>
    </div>
  );
}
