import { useQuery } from "@tanstack/react-query";
import { getUsage, POLL_MS, queryKeys, type Usage } from "../api/endpoints";

/** Plan, limits and usage of the tenant, polled like the monitor list. */
export function useUsage() {
  return useQuery({ queryKey: queryKeys.usage, queryFn: getUsage, refetchInterval: POLL_MS });
}

/** True when the tenant cannot create another monitor (unknown usage is not a limit). */
export function atMonitorLimit(usage: Usage | undefined): boolean {
  return usage !== undefined && usage.usage.monitors >= usage.limits.monitors;
}

/** True when the tenant cannot create another heartbeat (watchdog). */
export function atHeartbeatLimit(usage: Usage | undefined): boolean {
  return usage !== undefined && usage.usage.heartbeats >= usage.limits.heartbeats;
}

/** "3 monitors", "1 monitor": a count with its noun. */
export function countOf(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}
