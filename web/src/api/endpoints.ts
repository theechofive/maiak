import { ApiError, client, setCsrfToken, unwrap, type Schemas } from "./client";

export type SessionInfo = Schemas["SessionInfo"];
export type MonitorList = Schemas["MonitorList"];
export type MonitorListItem = Schemas["MonitorListItem"];
export type MonitorDetail = Schemas["MonitorDetail"];
export type MonitorCreate = Schemas["MonitorCreate"];
export type MonitorUpdate = Schemas["MonitorUpdate"];
export type MonitorStats = Schemas["MonitorStats"];
export type StatsRange = Schemas["StatsRange"];
export type LatencyPoint = Schemas["LatencyPoint"];
export type Check = Schemas["Check"];
export type Incident = Schemas["Incident"];
export type SlackSettings = Schemas["SlackSettings"];

/** Dashboard refresh interval (ADR-0015). TanStack Query pauses it while the tab is hidden. */
export const POLL_MS = 15_000;

export const queryKeys = {
  me: ["me"] as const,
  monitors: ["monitors"] as const,
  monitor: (id: string) => ["monitors", id] as const,
  stats: (id: string, range: StatsRange) => ["monitors", id, "stats", range] as const,
  checks: (id: string) => ["monitors", id, "checks"] as const,
  incidents: (id: string) => ["monitors", id, "incidents"] as const,
  slack: ["settings", "slack"] as const,
};

export async function login(login: string, password: string): Promise<SessionInfo> {
  const session = await unwrap(client.POST("/api/v1/auth/login", { body: { login, password } }));
  setCsrfToken(session.csrf_token);
  return session;
}

/** The current session, or null when not logged in. */
export async function getMe(): Promise<SessionInfo | null> {
  try {
    const session = await unwrap(client.GET("/api/v1/auth/me"));
    setCsrfToken(session.csrf_token);
    return session;
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      setCsrfToken(null);
      return null;
    }
    throw err;
  }
}

export async function logout(): Promise<void> {
  try {
    await unwrap(client.POST("/api/v1/auth/logout"));
  } finally {
    setCsrfToken(null);
  }
}

export const listMonitors = () => unwrap(client.GET("/api/v1/monitors"));

const monitorPath = (monitorId: string) => ({ params: { path: { monitorId } } });

export const getMonitor = (monitorId: string) =>
  unwrap(client.GET("/api/v1/monitors/{monitorId}", monitorPath(monitorId)));

export const createMonitor = (body: MonitorCreate) =>
  unwrap(client.POST("/api/v1/monitors", { body }));

export const updateMonitor = (monitorId: string, body: MonitorUpdate) =>
  unwrap(client.PATCH("/api/v1/monitors/{monitorId}", { ...monitorPath(monitorId), body }));

export const deleteMonitor = (monitorId: string) =>
  unwrap(client.DELETE("/api/v1/monitors/{monitorId}", monitorPath(monitorId)));

export const pauseMonitor = (monitorId: string) =>
  unwrap(client.POST("/api/v1/monitors/{monitorId}/pause", monitorPath(monitorId)));

export const resumeMonitor = (monitorId: string) =>
  unwrap(client.POST("/api/v1/monitors/{monitorId}/resume", monitorPath(monitorId)));

export const getMonitorStats = (monitorId: string, range: StatsRange) =>
  unwrap(
    client.GET("/api/v1/monitors/{monitorId}/stats", {
      params: { path: { monitorId }, query: { range } },
    }),
  );

export const listChecks = (monitorId: string, limit = 20) =>
  unwrap(
    client.GET("/api/v1/monitors/{monitorId}/checks", {
      params: { path: { monitorId }, query: { limit } },
    }),
  );

export const listIncidents = (monitorId: string) =>
  unwrap(client.GET("/api/v1/monitors/{monitorId}/incidents", monitorPath(monitorId)));

export const getSlackSettings = () => unwrap(client.GET("/api/v1/settings/slack"));

export const putSlackSettings = (webhookUrl: string) =>
  unwrap(client.PUT("/api/v1/settings/slack", { body: { webhookUrl } }));

export const testSlack = () => unwrap(client.POST("/api/v1/settings/slack/test"));
