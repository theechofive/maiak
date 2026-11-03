import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { errorMessage, errorMeta } from "../api/client";
import {
  listMonitors,
  POLL_MS,
  queryKeys,
  type MonitorList,
  type MonitorListItem,
  type Usage,
} from "../api/endpoints";
import {
  Badge,
  Button,
  buttonClass,
  cx,
  DotShape,
  ErrorState,
  Input,
  Skeleton,
  StatusIndicator,
  Sweep,
  Table,
  Td,
  Th,
  Tr,
  TriangleShape,
} from "../components/ui";
import { expectationText, isHeartbeat } from "../lib/heartbeat";
import { formatAgo, formatClock, formatMs, formatPercent, formatSince } from "../lib/format";
import {
  downReason,
  filterMonitors,
  monitorState,
  quietState,
  sortMonitors,
  statusText,
  summaryLine,
  summaryParts,
} from "../lib/monitors";
import { atHeartbeatLimit, atMonitorLimit, countOf, useUsage } from "../lib/usage";
import { withoutHttps } from "../lib/monitorForm";
import { useNow } from "../lib/useNow";

export default function Monitors() {
  const now = useNow();
  const [query, setQuery] = useState("");
  const filter = useRef<HTMLInputElement>(null);
  const monitors = useQuery({
    queryKey: queryKeys.monitors,
    queryFn: listMonitors,
    refetchInterval: POLL_MS,
  });
  const usage = useUsage();
  const down = monitors.data?.summary.down ?? 0;
  // The sweep marks refreshes, not the first load.
  const [firstLoad, setFirstLoad] = useState(0);
  if (firstLoad === 0 && monitors.dataUpdatedAt > 0) setFirstLoad(monitors.dataUpdatedAt);

  // A background tab still shows an outage in its title.
  useEffect(() => {
    document.title = down > 0 ? `▲ ${down} down · Maiak` : "Maiak";
    return () => {
      document.title = "Maiak";
    };
  }, [down]);

  const hasMonitors = (monitors.data?.items.length ?? 0) > 0;

  // "/" jumps to the filter from anywhere on the page, unless the user is typing.
  useEffect(() => {
    if (!hasMonitors) return;
    function onKey(e: globalThis.KeyboardEvent) {
      if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
      const typing =
        e.target instanceof Element &&
        e.target.closest("input, textarea, select, [contenteditable=true]");
      if (typing) return;
      e.preventDefault();
      filter.current?.focus();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [hasMonitors]);

  const list = monitors.data;

  return (
    <section className="flex flex-col">
      <div className="flex flex-wrap items-end justify-between gap-x-6 gap-y-3.5 pt-6 pb-5 sm:pt-10 sm:pb-6">
        <div className="flex min-w-0 flex-col gap-2">
          <h1 className="text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:text-display">
            Monitors
          </h1>
          {list && list.items.length > 0 && <Summary list={list} />}
          {monitors.isPending && <Skeleton className="w-60" />}
        </div>
        {list && list.items.length > 0 && (
          <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 max-sm:w-full">
            <div className="relative min-w-0 flex-1 sm:w-[280px] sm:flex-none">
              <Input
                ref={filter}
                type="search"
                aria-label="Filter by name or URL"
                placeholder="Filter by name or URL"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Escape") {
                    e.preventDefault();
                    if (query) setQuery("");
                    else e.currentTarget.blur();
                  } else if (e.key === "ArrowDown") {
                    const first = document.querySelector<HTMLElement>("[data-monitor-link]");
                    if (first) {
                      e.preventDefault();
                      first.focus();
                    }
                  }
                }}
                end={
                  // Always rendered: swapping the markup would remount the input and drop focus.
                  <kbd
                    className={cx(
                      "flex size-[18px] items-center justify-center border border-border font-mono text-micro max-sm:hidden",
                      query && "invisible",
                    )}
                  >
                    /
                  </kbd>
                }
              />
            </div>
            <AddMonitor usage={usage.data} />
          </div>
        )}
      </div>

      {monitors.isPending ? (
        <LoadingTable />
      ) : monitors.isError && !list ? (
        <ErrorState
          title="Cannot load monitors"
          detail={`${errorMessage(monitors.error)} Your monitors keep running.`}
          meta={errorMeta(monitors.error)}
          onRetry={() => void monitors.refetch()}
        />
      ) : list && list.items.length === 0 ? (
        <EmptyMonitors usage={usage.data} />
      ) : list ? (
        <MonitorTable
          list={list}
          query={query}
          now={now}
          clearQuery={() => {
            setQuery("");
            filter.current?.focus();
          }}
          focusFilter={() => filter.current?.focus()}
          sweepKey={
            monitors.dataUpdatedAt > firstLoad && firstLoad > 0 ? monitors.dataUpdatedAt : 0
          }
          updated={`Last updated ${formatClock(monitors.dataUpdatedAt)}${monitors.isError ? " · The last update failed, retrying" : ""}`}
        />
      ) : null}
    </section>
  );
}

function Summary({ list }: { list: MonitorList }) {
  return (
    <p className="text-[14px] text-muted-foreground">
      <span className="sr-only">{summaryLine(list)}</span>
      <span aria-hidden="true" className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
        {summaryParts(list).map((part, i) => (
          <span key={part.text} className="inline-flex items-center gap-2">
            {i > 0 && (
              <span aria-hidden="true" className="text-subtle-foreground">
                ·
              </span>
            )}
            {part.down ? (
              <span className="inline-flex items-center gap-1.5 font-semibold text-down">
                <TriangleShape />
                {part.text}
              </span>
            ) : (
              part.text
            )}
          </span>
        ))}
      </span>
    </p>
  );
}

function MonitorTable({
  list,
  query,
  now,
  clearQuery,
  focusFilter,
  sweepKey,
  updated,
}: {
  list: MonitorList;
  query: string;
  now: number;
  clearQuery: () => void;
  focusFilter: () => void;
  sweepKey: number;
  updated: string;
}) {
  const navigate = useNavigate();
  const visible = sortMonitors(filterMonitors(list.items, query));
  const hiddenDown = list.items.filter((m) => monitorState(m) === "down" && !visible.includes(m));
  const quiet = quietState(list);
  const downNames = sortMonitors(list.items)
    .filter((m) => monitorState(m) === "down")
    .map((m) => `${m.name} is down.`)
    .join(" ");

  // Arrow keys move between rows; up from the first row goes back to the filter.
  function onRowKeys(e: KeyboardEvent<HTMLElement>) {
    const links = [...e.currentTarget.querySelectorAll<HTMLElement>("[data-monitor-link]")];
    const i = links.indexOf(document.activeElement as HTMLElement);
    if (i < 0) return;
    const next = { ArrowDown: i + 1, ArrowUp: i - 1, Home: 0, End: links.length - 1 }[e.key];
    if (next === undefined) return;
    e.preventDefault();
    if (next < 0) focusFilter();
    else links[Math.min(next, links.length - 1)]?.focus();
  }

  return (
    <>
      {quiet && !query && (
        <div className="flex flex-col gap-1.5 pb-5">
          <span className="eyebrow flex items-center gap-2">
            <DotShape className="text-success" />
            All quiet
          </span>
          <span className="text-[20px] leading-[1.15] font-semibold tracking-[-0.015em] sm:text-headline">
            {quiet.title}
          </span>
          <span className="text-[14px] text-muted-foreground">{quiet.detail}</span>
        </div>
      )}

      <p aria-live="polite" className="sr-only">
        {downNames}
      </p>

      {hiddenDown.length > 0 && (
        <div
          role="status"
          className="flex flex-wrap items-center gap-2.5 border border-b-0 border-down-border bg-down-tint px-4 py-2.5 text-[13px]"
        >
          <TriangleShape className="text-down" />
          <span className="min-w-40 flex-1">
            {hiddenDown.length === 1
              ? `${hiddenDown[0]!.name} is down and hidden by this filter.`
              : `${hiddenDown.length} down monitors are hidden by this filter.`}
          </span>
          <Button variant="link" onClick={clearQuery}>
            Show all
          </Button>
        </div>
      )}

      {visible.length === 0 ? (
        <div className="flex flex-col items-start gap-2.5 border border-border px-6 pt-11 pb-12">
          <h2 className="text-[17px] font-semibold">No monitors match “{query.trim()}”.</h2>
          <p className="text-muted-foreground">
            Searched the names and URLs of {countOf(list.items.length, "monitor", "monitors")}.
          </p>
          <Button variant="secondary" size="small" className="mt-1" onClick={clearQuery}>
            Clear filter
          </Button>
        </div>
      ) : (
        <div className="relative overflow-hidden">
          <Sweep runKey={sweepKey} />
          <Table>
            <thead>
              <tr>
                <Th className="w-[132px] max-sm:w-auto">Status</Th>
                <Th>Monitor</Th>
                <Th numeric className="max-sm:hidden">
                  Uptime 24h
                </Th>
                <Th numeric className="max-sm:hidden">
                  Resp
                </Th>
                <Th numeric className="max-sm:hidden">
                  Checked
                </Th>
              </tr>
            </thead>
            {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions -- arrow keys move focus between the row links, which are the focusable elements. */}
            <tbody onKeyDown={onRowKeys}>
              {visible.map((m) => (
                <MonitorRow
                  key={m.id}
                  monitor={m}
                  now={now}
                  onOpen={() => navigate(`/monitors/${m.id}`)}
                />
              ))}
            </tbody>
          </Table>
        </div>
      )}

      <div className="flex flex-wrap items-center justify-between gap-4 px-px py-2 font-mono text-mono text-muted-foreground">
        <span>{updated}</span>
        {visible.length > 0 && (
          <span aria-hidden="true" className="flex items-center gap-3.5 max-sm:hidden">
            <span className="flex items-center gap-1.5">
              <Key>↑</Key>
              <Key>↓</Key>move
            </span>
            <span className="flex items-center gap-1.5">
              <Key>Enter</Key>open
            </span>
            <span className="flex items-center gap-1.5">
              <Key>/</Key>filter
            </span>
          </span>
        )}
      </div>
    </>
  );
}

function Key({ children }: { children: string }) {
  return (
    <kbd className="inline-flex h-[18px] min-w-[18px] items-center justify-center border border-border px-1 font-mono text-micro">
      {children}
    </kbd>
  );
}

function MonitorRow({
  monitor: m,
  now,
  onOpen,
}: {
  monitor: MonitorListItem;
  now: number;
  onOpen: () => void;
}) {
  const state = monitorState(m);
  const isDown = state === "down";
  const heartbeat = isHeartbeat(m);
  const checked = formatAgo(heartbeat ? m.lastPingAt : m.lastCheckedAt, now);
  const figure = cx("text-mono", isDown ? "font-medium text-down" : "text-muted-foreground");
  return (
    <Tr tone={isDown ? "down" : "default"} interactive onClick={onOpen}>
      <Td className={cx("align-top sm:align-middle", isDown && "py-3.5")}>
        <StatusIndicator status={state} label={statusText(m, now)} />
      </Td>
      <Td className={cx("min-w-0", isDown && "py-3.5")}>
        <div className="flex min-w-0 flex-col gap-1">
          <span className="flex min-w-0 flex-wrap items-baseline gap-x-2.5 gap-y-1">
            <Link
              to={`/monitors/${m.id}`}
              data-monitor-link
              className={cx(
                "font-medium hover:underline",
                isDown ? "text-[15px] font-semibold" : "text-[14.5px]",
                state === "paused" && "text-muted-foreground",
              )}
              onClick={(e) => e.stopPropagation()}
            >
              {m.name}
            </Link>
            {heartbeat && <Badge tone="strong">Watchdog</Badge>}
            <span className="min-w-0 truncate font-mono text-mono text-subtle-foreground">
              {heartbeat ? expectationText(m) : withoutHttps(m.url)}
            </span>
          </span>
          {isDown && (
            <span className="text-caption">
              {downReason(m)} · since {formatSince(m.statusSince, now)}
            </span>
          )}
          <span className="font-mono text-mono text-muted-foreground sm:hidden">
            {[formatPercent(m.uptime24h), formatMs(m.lastResponseMs), checked]
              .filter((part) => part !== "—")
              .join(" · ")}
          </span>
        </div>
      </Td>
      <Td numeric className={cx(figure, "max-sm:hidden")}>
        {formatPercent(m.uptime24h)}
      </Td>
      <Td numeric className={cx(figure, "max-sm:hidden")}>
        {formatMs(m.lastResponseMs)}
      </Td>
      <Td numeric className="text-subtle-foreground max-sm:hidden">
        {checked}
      </Td>
    </Tr>
  );
}

function LoadingTable() {
  return (
    <Table>
      <thead>
        <tr>
          <Th className="w-[132px]">Status</Th>
          <Th>Monitor</Th>
          <Th numeric>Checked</Th>
        </tr>
      </thead>
      <tbody aria-busy="true">
        {["60%", "42%", "70%", "50%", "64%"].map((width) => (
          <Tr key={width}>
            <Td>
              <Skeleton className="w-12" />
            </Td>
            <Td>
              <span className="block" style={{ width }}>
                <Skeleton />
              </span>
            </Td>
            <Td>
              <Skeleton className="ml-auto w-12" />
            </Td>
          </Tr>
        ))}
        <tr>
          <td colSpan={3} className="px-4 py-2 font-mono text-mono text-muted-foreground">
            Loading monitors…
          </td>
        </tr>
      </tbody>
    </Table>
  );
}

function EmptyMonitors({ usage }: { usage: Usage | undefined }) {
  return (
    <div className="flex flex-col gap-7 border border-border px-5 pt-10 pb-11 sm:px-10">
      <div aria-hidden="true" className="flex flex-col gap-1.5">
        <div className="flex h-14 items-end justify-end border-b border-dashed border-input">
          <span className="h-[22px] w-0.5 bg-foreground" />
        </div>
        <div className="flex justify-between font-mono text-micro text-subtle-foreground">
          <span>24 h ago</span>
          <span className="text-muted-foreground">Now</span>
        </div>
      </div>
      <div className="flex max-w-[560px] flex-col gap-2.5">
        <h2 className="text-[28px] leading-[1.15] font-semibold tracking-[-0.02em]">
          Start your first monitor
        </h2>
        <p className="text-[14.5px] text-muted-foreground">
          Maiak checks a URL every minute, or waits for a heartbeat from your jobs, and alerts you
          in Slack when something fails.
        </p>
      </div>
      <div className="grid max-w-[640px] gap-px bg-border-faint sm:grid-cols-2">
        <div className="flex flex-col items-start gap-1.5 bg-background px-4 py-3.5">
          <Badge>HTTP</Badge>
          <span className="text-[14px] font-medium">Check a URL</span>
          <span className="text-caption text-muted-foreground">
            Alerts when it stops answering or returns an error.
            {usage && (
              <span className="mt-1 block">
                Your {usage.plan.name} plan includes{" "}
                {countOf(usage.limits.monitors, "monitor", "monitors")}.
              </span>
            )}
          </span>
        </div>
        <div className="flex flex-col items-start gap-1.5 bg-background px-4 py-3.5">
          <Badge>Watchdog</Badge>
          <span className="text-[14px] font-medium">Watch a job</span>
          <span className="text-caption text-muted-foreground">
            Your job or Alertmanager&apos;s Watchdog alert pings Maiak; it alerts when the pings
            stop.
            {usage && (
              <span className="mt-1 block">
                Your plan includes {countOf(usage.limits.heartbeats, "watchdog", "watchdogs")}.
              </span>
            )}
          </span>
        </div>
      </div>
      <AddMonitor usage={usage} />
    </div>
  );
}

/** True when neither another HTTP monitor nor another watchdog fits the plan. */
function atAllLimits(usage: Usage | undefined): boolean {
  return atMonitorLimit(usage) && atHeartbeatLimit(usage);
}

/**
 * The add link, or when both of the plan's limits (HTTP monitors and watchdogs) are reached a
 * button marked aria-disabled with the reason next to it (the server enforces the limits anyway).
 */
function AddMonitor({ usage }: { usage: Usage | undefined }) {
  if (!usage || !atAllLimits(usage)) {
    return (
      <Link to="/monitors/new" className={cx(buttonClass(), "self-start")}>
        Add monitor
      </Link>
    );
  }
  return (
    <div className="flex flex-wrap items-center gap-3">
      <span id="monitor-limit" className="max-w-[320px] text-caption text-muted-foreground">
        The {usage.plan.name} plan allows {countOf(usage.limits.monitors, "monitor", "monitors")}{" "}
        and {countOf(usage.limits.heartbeats, "watchdog", "watchdogs")}. Delete one to add a new
        one.
      </span>
      <Button aria-disabled="true" aria-describedby="monitor-limit">
        Add monitor
      </Button>
    </div>
  );
}
