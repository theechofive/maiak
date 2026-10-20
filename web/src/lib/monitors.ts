import type { Check, MonitorDetail, MonitorList, MonitorListItem } from "../api/endpoints";
import { describeErrorKind, formatDuration, formatSince, secondsBetween } from "./format";

type StatusFields = Pick<MonitorListItem, "paused" | "status" | "statusSince">;

/** "Paused", "Pending", "Up" or "Down 12 min". */
export function statusText(monitor: StatusFields, now: number): string {
  if (monitor.paused) return "Paused";
  if (monitor.status === "unknown") return "Pending";
  if (monitor.status === "up") return "Up";
  return `Down ${formatDuration(secondsBetween(monitor.statusSince, now))}`;
}

/** "3 monitors · 1 up · 1 down · 1 paused" (pending ones are added only when there are some). */
export function summaryLine(list: MonitorList): string {
  const { up, down, paused, unknown } = list.summary;
  const total = list.items.length;
  const parts = [
    `${total} ${total === 1 ? "monitor" : "monitors"}`,
    `${up} up`,
    `${down} down`,
    `${paused} paused`,
  ];
  if (unknown > 0) parts.push(`${unknown} pending`);
  return parts.join(" · ");
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
