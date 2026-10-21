import type { MonitorListItem } from "../api/endpoints";
import { formatAgo, formatDuration, formatSince, secondsBetween } from "./format";

/** Bounds of a heartbeat (ADR-0094); the server checks them too. */
export const INTERVAL_MIN = 60;
export const INTERVAL_MAX = 86_400;
export const GRACE_MAX = 3_600;
export const DEFAULT_INTERVAL = 300;
export const DEFAULT_GRACE = 60;

export const INTERVAL_PRESETS = [
  { label: "1 min", seconds: 60 },
  { label: "5 min", seconds: 300 },
  { label: "15 min", seconds: 900 },
] as const;

export const isHeartbeat = (m: Pick<MonitorListItem, "type">) => m.type === "heartbeat";

/** "Ping every 5 min, grace 1 min". */
export function expectationText(
  m: Pick<MonitorListItem, "expectedIntervalSeconds" | "graceSeconds">,
): string {
  return `Ping every ${formatDuration(m.expectedIntervalSeconds ?? 0)}, grace ${formatDuration(m.graceSeconds ?? 0)}`;
}

/** The status line of a heartbeat's details page. */
export function heartbeatStatusLine(
  m: Pick<MonitorListItem, "paused" | "status" | "statusSince" | "lastPingAt">,
  now: number,
): string {
  if (m.paused) return "Paused. Pings are ignored until you resume the monitor.";
  if (m.status === "unknown") return "Waiting for the first ping";
  const duration = formatDuration(secondsBetween(m.statusSince, now));
  const since = `since ${formatSince(m.statusSince, now)}`;
  if (m.status === "up") return `Up for ${duration} · last ping ${formatAgo(m.lastPingAt, now)}`;
  return `Down for ${duration} · no ping in time · ${since}`;
}

/**
 * Alertmanager's repeat interval for a heartbeat: half the expected interval, at least 30 s, so a
 * lost ping does not make the heartbeat late.
 */
export function repeatInterval(expectedSeconds: number): string {
  return `${Math.max(30, Math.floor(expectedSeconds / 2))}s`;
}

/** The Alertmanager route and receiver for the Watchdog alert; the URL itself lives in a file. */
export function alertmanagerSnippet(expectedSeconds: number): string {
  const repeat = repeatInterval(expectedSeconds);
  return `route:
  routes:
    - receiver: maiak-heartbeat
      matchers:
        - alertname = "Watchdog"
      group_wait: 0s
      group_interval: ${repeat}
      repeat_interval: ${repeat}

receivers:
  - name: maiak-heartbeat
    webhook_configs:
      # The URL is a secret: keep it in a file, not in this config.
      - url_file: /etc/alertmanager/secrets/maiak-heartbeat/url
        send_resolved: false`;
}

/** A Kubernetes Secret holding the URL for url_file. */
export function secretCommand(url: string): string {
  return `kubectl -n monitoring create secret generic maiak-heartbeat --from-literal=url='${url}'`;
}
