import type { Check, MonitorDetail, MonitorList, MonitorListItem } from "../api/endpoints";
import {
  describeErrorKind,
  formatDuration,
  formatPercent,
  formatSince,
  secondsBetween,
} from "./format";

type StatusFields = Pick<MonitorListItem, "paused" | "status" | "statusSince">;

/** "Paused", "Pending", "Up" or "Down 12 min". */
export function statusText(monitor: StatusFields, now: number): string {
  if (monitor.paused) return "Paused";
  if (monitor.status === "unknown") return "Pending";
  if (monitor.status === "up") return "Up";
  return `Down ${formatDuration(secondsBetween(monitor.statusSince, now))}`;
}

export type MonitorState = "up" | "down" | "paused" | "pending";

/** The state a status indicator shows: paused wins over the last known status. */
export function monitorState(monitor: Pick<MonitorListItem, "paused" | "status">): MonitorState {
  if (monitor.paused) return "paused";
  if (monitor.status === "unknown") return "pending";
  return monitor.status;
}

/** The parts of the summary line; `down` marks the part that needs attention. */
export function summaryParts(list: MonitorList): { text: string; down: boolean }[] {
  const { up, down, paused, unknown } = list.summary;
  const total = list.items.length;
  const parts = [
    { text: `${total} ${total === 1 ? "monitor" : "monitors"}`, down: false },
    { text: `${up} up`, down: false },
    { text: `${down} down`, down: down > 0 },
    { text: `${paused} paused`, down: false },
  ];
  if (unknown > 0) parts.push({ text: `${unknown} pending`, down: false });
  return parts;
}

/** "3 monitors · 1 up · 1 down · 1 paused" (pending ones are added only when there are some). */
export function summaryLine(list: MonitorList): string {
  return summaryParts(list)
    .map((p) => p.text)
    .join(" · ");
}

/** Down first, the newest failure on top; paused last; the rest by name. */
export function sortMonitors<
  T extends Pick<MonitorListItem, "name" | "paused" | "status" | "statusSince">,
>(items: T[]): T[] {
  const rank = (m: T) => (m.paused ? 2 : m.status === "down" ? 0 : 1);
  return [...items].sort(
    (a, b) =>
      rank(a) - rank(b) ||
      (rank(a) === 0 ? Date.parse(b.statusSince) - Date.parse(a.statusSince) : 0) ||
      a.name.localeCompare(b.name),
  );
}

/**
 * The calm headline when nothing is down, from the list's own figures; null while something is
 * down or when every monitor is paused.
 */
export function quietState(list: MonitorList): { title: string; detail: string } | null {
  const active = list.items.filter((m) => !m.paused);
  if (list.summary.down > 0 || active.length === 0) return null;
  const title = list.summary.unknown > 0 ? "Nothing is down." : "Everything is up.";
  const measured = active.filter((m) => m.uptime24h != null);
  const lowest = measured.reduce<(typeof measured)[number] | undefined>(
    (low, m) => (low === undefined || m.uptime24h! < low.uptime24h! ? m : low),
    undefined,
  );
  if (!lowest || lowest.uptime24h! >= 100) {
    return { title, detail: "No failed checks in the last 24 hours." };
  }
  return {
    title,
    detail: `Lowest uptime in the last 24 hours: ${formatPercent(lowest.uptime24h)}, ${lowest.name}.`,
  };
}

/** Case-insensitive match on name or URL; an empty query keeps everything. */
export function filterMonitors<T extends Pick<MonitorListItem, "name" | "url">>(
  items: T[],
  query: string,
): T[] {
  const q = query.trim().toLowerCase();
  if (!q) return items;
  return items.filter((m) => m.name.toLowerCase().includes(q) || m.url.toLowerCase().includes(q));
}

/** Why a monitor is down, in words: "Timed out after 10 s", "DNS lookup failed". */
export function downReason(
  monitor: Pick<MonitorListItem, "lastErrorKind" | "timeoutSeconds">,
): string {
  if (monitor.lastErrorKind === "timeout") return `Timed out after ${monitor.timeoutSeconds} s`;
  return describeErrorKind(monitor.lastErrorKind);
}

/**
 * The status line of the details page, e.g. "Down for 12 min · HTTP 503 · since 14:02". The HTTP
 * code comes from the latest check when it failed with one; otherwise the error kind is shown.
 */
export function statusLine(
  monitor: Pick<MonitorDetail, "paused" | "status" | "statusSince" | "lastErrorKind">,
  latestCheck: Check | undefined,
  now: number,
): string {
  if (monitor.paused) return "Paused. Checks are stopped until you resume the monitor.";
  if (monitor.status === "unknown") return "Waiting for the first check";
  const duration = formatDuration(secondsBetween(monitor.statusSince, now));
  const since = `since ${formatSince(monitor.statusSince, now)}`;
  if (monitor.status === "up") return `Up for ${duration} · ${since}`;
  const reason =
    latestCheck && !latestCheck.ok && latestCheck.httpStatus != null
      ? `HTTP ${latestCheck.httpStatus}`
      : describeErrorKind(monitor.lastErrorKind);
  return `Down for ${duration} · ${reason} · ${since}`;
}

/** How many of the latest checks failed in a row, and the newest success among the loaded ones. */
export function failureStreak(checks: Check[]): {
  count: number;
  all: boolean;
  lastSuccess?: Check;
} {
  const i = checks.findIndex((c) => c.ok);
  return i < 0
    ? { count: checks.length, all: true }
    : { count: i, all: false, lastSuccess: checks[i] };
}

/** "every 60 s · 10 s timeout · expects 2xx · alerts after 2 failed checks". */
export function checkSettings(
  m: Pick<
    MonitorDetail,
    "intervalSeconds" | "timeoutSeconds" | "expectedStatus" | "failureThreshold"
  >,
): string {
  const expects = m.expectedStatus === "2xx" ? "expects 2xx" : `expects HTTP ${m.expectedStatus}`;
  const after =
    m.failureThreshold === 1
      ? "alerts on the first failure"
      : `alerts after ${m.failureThreshold} failed checks`;
  return `every ${m.intervalSeconds} s · ${m.timeoutSeconds} s timeout · ${expects} · ${after}`;
}
