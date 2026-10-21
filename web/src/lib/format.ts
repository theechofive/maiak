// The one place that turns durations, times and numbers into text. Times are shown in the
// browser's local time zone; the copy is English, so the locale is fixed.

const MINUTE = 60;
const HOUR = 3_600;
const DAY = 86_400;

/** A duration in seconds as at most two units, e.g. "45 s", "12 min", "1 h 30 min", "2 d 3 h". */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null) return "ongoing";
  const total = Math.max(0, Math.floor(seconds));
  const days = Math.floor(total / DAY);
  const hours = Math.floor((total % DAY) / HOUR);
  const minutes = Math.floor((total % HOUR) / MINUTE);
  if (days > 0) return hours > 0 ? `${days} d ${hours} h` : `${days} d`;
  if (hours > 0) return minutes > 0 ? `${hours} h ${minutes} min` : `${hours} h`;
  if (minutes > 0) return `${minutes} min`;
  return `${total} s`;
}

const toMs = (value: string | number | Date) =>
  typeof value === "number" ? value : new Date(value).getTime();

/** Seconds from `from` to `to` (both ISO strings, epoch ms or dates). */
export function secondsBetween(from: string | number | Date, to: string | number | Date): number {
  return (toMs(to) - toMs(from)) / 1000;
}

/** Relative past time with one unit: "40 s ago", "12 min ago", "3 h ago", "2 d ago". */
export function formatAgo(iso: string | null | undefined, now: number): string {
  if (!iso) return "never";
  const s = Math.max(0, Math.floor(secondsBetween(iso, now)));
  if (s < MINUTE) return `${s} s ago`;
  if (s < HOUR) return `${Math.floor(s / MINUTE)} min ago`;
  if (s < DAY) return `${Math.floor(s / HOUR)} h ago`;
  return `${Math.floor(s / DAY)} d ago`;
}

const timeFormat = new Intl.DateTimeFormat("en-US", {
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});
const clockFormat = new Intl.DateTimeFormat("en-US", {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hourCycle: "h23",
});
const dateTimeFormat = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});
const dateFormat = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric" });

/** "14:02" */
export const formatTime = (value: string | number | Date) => timeFormat.format(toMs(value));
/** "14:02:31" */
export const formatClock = (value: string | number | Date) => clockFormat.format(toMs(value));
/** "Oct 20, 14:02" */
export const formatDateTime = (value: string | number | Date) => dateTimeFormat.format(toMs(value));
/** "Oct 20" */
export const formatDate = (value: string | number | Date) => dateFormat.format(toMs(value));

/** The time only when it is today, otherwise date and time. */
export function formatSince(iso: string, now: number): string {
  const d = new Date(iso);
  const today = new Date(now);
  const sameDay =
    d.getFullYear() === today.getFullYear() &&
    d.getMonth() === today.getMonth() &&
    d.getDate() === today.getDate();
  return sameDay ? formatTime(d) : formatDateTime(d);
}

/** "99.95%", "100%", or a dash when there is no data. */
export function formatPercent(value: number | null | undefined): string {
  if (value == null) return "—";
  return `${Number(value.toFixed(2))}%`;
}

/** "245 ms", or a dash when there is no data. */
export function formatMs(value: number | null | undefined): string {
  if (value == null) return "—";
  return `${Math.round(value)} ms`;
}

/** Whole days from now until the given time (negative when it is in the past). */
export function daysUntil(iso: string, now: number): number {
  return Math.floor(secondsBetween(now, iso) / DAY);
}

/** TLS expiry card text: "Expired", "today" under 24 h, "in N h" under 48 h, then whole days. */
export function formatTlsExpiry(iso: string, now: number): string {
  const s = secondsBetween(now, iso);
  if (s < 0) return "Expired";
  if (s < DAY) return "today";
  if (s < 2 * DAY) return `in ${Math.floor(s / HOUR)} h`;
  const days = Math.floor(s / DAY);
  return `in ${days} ${days === 1 ? "day" : "days"}`;
}

const errorKinds: Record<string, string> = {
  dns: "DNS lookup failed",
  connect: "Connection failed",
  tls: "TLS error",
  timeout: "Timeout",
  blocked_address: "Blocked address",
  bad_port: "Port not allowed",
  redirect: "Redirect error",
  unexpected_status: "Unexpected status",
  other: "Other error",
  heartbeat_missed: "Heartbeat missed",
  recovered: "Recovered",
  url_changed: "URL changed",
  paused: "Paused",
  deleted: "Deleted",
};

/** Human text for a check error kind or incident reason; unknown kinds are shown as they are. */
export function describeErrorKind(kind: string | null | undefined): string {
  if (!kind) return "—";
  return errorKinds[kind] ?? kind;
}
