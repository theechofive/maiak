import type { MonitorList, MonitorListItem } from "../api/endpoints";
import { formatDuration, secondsBetween } from "./format";

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
