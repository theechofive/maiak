import { useQuery } from "@tanstack/react-query";
import { getUsage, POLL_MS, queryKeys, type Usage } from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import { isUnlimited } from "./limits";
import { abilities } from "./roles";

/**
 * Plan, limits and usage of the tenant, polled like the monitor list. Owners and admins only
 * (L-288): for members and viewers it never loads, so limit hints stay off and a reached limit
 * shows up as the create's own "plan limit reached" message.
 */
export function useUsage() {
  const { session } = useAuth();
  return useQuery({
    queryKey: queryKeys.usage,
    queryFn: getUsage,
    refetchInterval: POLL_MS,
    enabled: abilities(session).seeUsage,
  });
}

/** True when the tenant cannot create another monitor (unknown usage is not a limit). */
export function atMonitorLimit(usage: Usage | undefined): boolean {
  return usage !== undefined && usage.usage.monitors >= usage.limits.monitors;
}

/** True when the tenant cannot create another heartbeat (watchdog). */
export function atHeartbeatLimit(usage: Usage | undefined): boolean {
  return usage !== undefined && usage.usage.heartbeats >= usage.limits.heartbeats;
}

/** Plans the platform admin cannot assign yet: `community` waits for self-hosted packaging (L-312). */
export const HIDDEN_PLANS: readonly string[] = ["community"];

/** "3 monitors", "1 monitor", "unlimited monitors": a count or limit with its noun. */
export function countOf(n: number, one: string, many: string): string {
  if (isUnlimited(n)) return `unlimited ${many}`;
  return `${n} ${n === 1 ? one : many}`;
}
