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
export type Channel = Schemas["Channel"];
export type ChannelCreate = Schemas["ChannelCreate"];
export type ChannelUpdate = Schemas["ChannelUpdate"];
export type ChannelEvent = Schemas["ChannelEvent"];
export type Usage = Schemas["Usage"];
export type PublicConfig = Schemas["PublicConfig"];
export type AdminStats = Schemas["AdminStats"];
export type AdminTenant = Schemas["AdminTenant"];
export type AdminTenantList = Schemas["AdminTenantList"];
export type AdminTenantUpdate = Schemas["AdminTenantUpdate"];
export type AdminUser = Schemas["AdminUser"];
export type AdminUserList = Schemas["AdminUserList"];
export type MembershipRole = Schemas["MembershipRole"];
export type Organization = Schemas["Organization"];
export type Member = Schemas["Member"];
export type Invitation = Schemas["Invitation"];
export type InvitationRole = Schemas["InvitationRole"];
export type InvitationCreated = Schemas["InvitationCreated"];
export type InvitationPreview = Schemas["InvitationPreview"];
export type Project = Schemas["Project"];
export type ProjectMember = Schemas["ProjectMember"];
export type ProjectRole = Schemas["ProjectRole"];
export type StatusPage = Schemas["StatusPage"];
export type StatusPageCreate = Schemas["StatusPageCreate"];
export type StatusPageUpdate = Schemas["StatusPageUpdate"];
export type StatusPageComponent = Schemas["StatusPageComponent"];
export type StatusPageComponentInput = Schemas["StatusPageComponentInput"];
export type StatusPageSnapshot = Schemas["StatusPageSnapshot"];
export type OAuthProvider = Schemas["OAuthProvider"];
export type AuthProviders = Schemas["AuthProviders"];
export type Identity = Schemas["Identity"];
export type IdentityList = Schemas["IdentityList"];
export type AuthDomain = Schemas["AuthDomain"];
export type AuthDomainRole = Schemas["AuthDomainRole"];
export type AuthDomainUpdate = Schemas["AuthDomainUpdate"];

/** Page size of the admin lists (the API default; at most 100). */
export const ADMIN_PAGE_SIZE = 25;

/** Dashboard refresh interval (ADR-0015). TanStack Query pauses it while the tab is hidden. */
export const POLL_MS = 15_000;

export const queryKeys = {
  me: ["me"] as const,
  monitors: ["monitors"] as const,
  monitorList: (projectId: string | undefined) => ["monitors", "list", projectId ?? "all"] as const,
  monitor: (id: string) => ["monitors", id] as const,
  stats: (id: string, range: StatsRange) => ["monitors", id, "stats", range] as const,
  checks: (id: string) => ["monitors", id, "checks"] as const,
  incidents: (id: string) => ["monitors", id, "incidents"] as const,
  channels: ["channels"] as const,
  channelsFor: (projectId: string | undefined) => ["channels", "for", projectId ?? "all"] as const,
  projects: ["projects"] as const,
  projectMembers: (id: string) => ["projects", id, "members"] as const,
  usage: ["usage"] as const,
  statusPages: ["status-pages"] as const,
  statusPageComponents: (id: string) => ["status-pages", id, "components"] as const,
  statusPagePreview: (id: string) => ["status-pages", id, "preview"] as const,
  publicConfig: ["public-config"] as const,
  authProviders: ["auth-providers"] as const,
  identities: ["identities"] as const,
  authDomains: ["auth-domains"] as const,
  adminStats: ["admin", "stats"] as const,
  adminTenants: (query: string, offset: number, includeDeleted: boolean) =>
    ["admin", "tenants", { query, offset, includeDeleted }] as const,
  adminTenant: (id: string) => ["admin", "tenant", id] as const,
  adminUsers: (id: string) => ["admin", "tenant", id, "users"] as const,
  organizations: ["organizations"] as const,
  members: ["members"] as const,
  invitations: ["invitations"] as const,
  invitationPreview: (token: string) => ["invitation-preview", token] as const,
};

export async function login(login: string, password: string): Promise<SessionInfo> {
  const session = await unwrap(client.POST("/api/v1/auth/login", { body: { login, password } }));
  setCsrfToken(session.csrf_token);
  return session;
}

/**
 * Registers a new account and workspace; the answer is a session, exactly like login. With an
 * invitation token the account joins the inviting organization instead (ADR-0102).
 */
export async function register(
  email: string,
  password: string,
  inviteToken?: string,
): Promise<SessionInfo> {
  const body = inviteToken ? { email, password, inviteToken } : { email, password };
  const session = await unwrap(client.POST("/api/v1/auth/register", { body }));
  setCsrfToken(session.csrf_token);
  return session;
}

/** Settings the app needs before login (no session). */
export const getPublicConfig = () => unwrap(client.GET("/api/v1/public/config"));

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

/** The monitors the user sees; with a project id only that project's. */
export const listMonitors = (projectId?: string) =>
  unwrap(client.GET("/api/v1/monitors", { params: { query: projectId ? { projectId } : {} } }));

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

/** A new secret URL for a heartbeat; the old one stops working. Shown once. */
export const regenerateHeartbeatToken = (monitorId: string) =>
  unwrap(client.POST("/api/v1/monitors/{monitorId}/heartbeat-token", monitorPath(monitorId)));

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

/** The channels the user sees; with a project id, those a monitor of that project may select. */
export const listChannels = (projectId?: string) =>
  unwrap(client.GET("/api/v1/channels", { params: { query: projectId ? { projectId } : {} } }));

const channelPath = (channelId: string) => ({ params: { path: { channelId } } });

export const createChannel = (body: ChannelCreate) =>
  unwrap(client.POST("/api/v1/channels", { body }));

export const updateChannel = (channelId: string, body: ChannelUpdate) =>
  unwrap(client.PATCH("/api/v1/channels/{channelId}", { ...channelPath(channelId), body }));

export const deleteChannel = (channelId: string) =>
  unwrap(client.DELETE("/api/v1/channels/{channelId}", channelPath(channelId)));

export const testChannel = (channelId: string) =>
  unwrap(client.POST("/api/v1/channels/{channelId}/test", channelPath(channelId)));

export const getUsage = () => unwrap(client.GET("/api/v1/usage"));

// Platform admin panel (P-04, ADR-0097). Every call answers 403 to anybody but the platform admin.

export const getAdminStats = () => unwrap(client.GET("/api/v1/admin/stats"));

export const listAdminTenants = (query: string, offset: number, includeDeleted: boolean) =>
  unwrap(
    client.GET("/api/v1/admin/tenants", {
      params: { query: { query, offset, limit: ADMIN_PAGE_SIZE, includeDeleted } },
    }),
  );

const tenantPath = (tenantId: string) => ({ params: { path: { tenantId } } });

export const getAdminTenant = (tenantId: string) =>
  unwrap(client.GET("/api/v1/admin/tenants/{tenantId}", tenantPath(tenantId)));

export const updateAdminTenant = (tenantId: string, body: AdminTenantUpdate) =>
  unwrap(client.PATCH("/api/v1/admin/tenants/{tenantId}", { ...tenantPath(tenantId), body }));

export const deleteAdminTenant = (tenantId: string) =>
  unwrap(client.DELETE("/api/v1/admin/tenants/{tenantId}", tenantPath(tenantId)));

/** The members of a tenant (the first page is enough for the panel: tenants have one owner). */
export const listAdminTenantUsers = (tenantId: string) =>
  unwrap(
    client.GET("/api/v1/admin/tenants/{tenantId}/users", {
      params: { path: { tenantId }, query: { limit: 100 } },
    }),
  );

/** A new temporary password for the user, returned once; all of the user's sessions end. */
export const resetAdminUserPassword = (userId: string) =>
  unwrap(
    client.POST("/api/v1/admin/users/{userId}/reset-password", {
      params: { path: { userId } },
    }),
  );

// Organizations, members and invitations (T-01, ADR-0102).

export const listOrganizations = () => unwrap(client.GET("/api/v1/auth/organizations"));

/** Replaces the session with one in another organization (new cookie and CSRF token). */
export async function switchOrganization(tenantId: string): Promise<SessionInfo> {
  const session = await unwrap(
    client.POST("/api/v1/auth/switch-organization", { body: { tenantId } }),
  );
  setCsrfToken(session.csrf_token);
  return session;
}

export const listMembers = () => unwrap(client.GET("/api/v1/members"));

const memberPath = (userId: string) => ({ params: { path: { userId } } });

export const updateMember = (userId: string, role: MembershipRole) =>
  unwrap(client.PATCH("/api/v1/members/{userId}", { ...memberPath(userId), body: { role } }));

export const removeMember = (userId: string) =>
  unwrap(client.DELETE("/api/v1/members/{userId}", memberPath(userId)));

export const listInvitations = () => unwrap(client.GET("/api/v1/invitations"));

/** A new invitation; its link is in this answer only. */
export const createInvitation = (role: InvitationRole, label: string) =>
  unwrap(client.POST("/api/v1/invitations", { body: label ? { role, label } : { role } }));

export const revokeInvitation = (invitationId: string) =>
  unwrap(
    client.POST("/api/v1/invitations/{invitationId}/revoke", {
      params: { path: { invitationId } },
    }),
  );

/** What an invitation link offers; works without a session. */
export const lookupInvitation = (token: string) =>
  unwrap(client.POST("/api/v1/invitations/lookup", { body: { token } }));

/** Joins the inviting organization as the signed-in user; the session moves there. */
export async function acceptInvitation(token: string): Promise<SessionInfo> {
  const session = await unwrap(client.POST("/api/v1/invitations/accept", { body: { token } }));
  setCsrfToken(session.csrf_token);
  return session;
}

export async function leaveOrganization(): Promise<void> {
  try {
    await unwrap(client.POST("/api/v1/organization/leave"));
  } finally {
    setCsrfToken(null);
  }
}

export const deleteOrganization = () => unwrap(client.DELETE("/api/v1/organization"));

// Projects (T-02, ADR-0102).

export const listProjects = () => unwrap(client.GET("/api/v1/projects"));

const projectPath = (projectId: string) => ({ params: { path: { projectId } } });

export const createProject = (name: string) =>
  unwrap(client.POST("/api/v1/projects", { body: { name } }));

export const renameProject = (projectId: string, name: string) =>
  unwrap(
    client.PATCH("/api/v1/projects/{projectId}", { ...projectPath(projectId), body: { name } }),
  );

export const deleteProject = (projectId: string) =>
  unwrap(client.DELETE("/api/v1/projects/{projectId}", projectPath(projectId)));

export const listProjectMembers = (projectId: string) =>
  unwrap(client.GET("/api/v1/projects/{projectId}/members", projectPath(projectId)));

/** Adds a member to a project; "viewer" makes it read-only there, undefined keeps its role. */
export const putProjectMember = (projectId: string, userId: string, role?: ProjectRole) =>
  unwrap(
    client.PUT("/api/v1/projects/{projectId}/members/{userId}", {
      params: { path: { projectId, userId } },
      body: role ? { role } : {},
    }),
  );

export const removeProjectMember = (projectId: string, userId: string) =>
  unwrap(
    client.DELETE("/api/v1/projects/{projectId}/members/{userId}", {
      params: { path: { projectId, userId } },
    }),
  );

// Status page (P-05, ADR-0103). Every role reads the settings; admins and owners change them.

export const listStatusPages = () => unwrap(client.GET("/api/v1/status-pages"));

const statusPagePath = (statusPageId: string) => ({ params: { path: { statusPageId } } });

export const createStatusPage = (body: StatusPageCreate) =>
  unwrap(client.POST("/api/v1/status-pages", { body }));

export const updateStatusPage = (statusPageId: string, body: StatusPageUpdate) =>
  unwrap(
    client.PATCH("/api/v1/status-pages/{statusPageId}", { ...statusPagePath(statusPageId), body }),
  );

export const deleteStatusPage = (statusPageId: string) =>
  unwrap(client.DELETE("/api/v1/status-pages/{statusPageId}", statusPagePath(statusPageId)));

/** Write-only: the password is never returned. */
export const setStatusPagePassword = (statusPageId: string, password: string) =>
  unwrap(
    client.PUT("/api/v1/status-pages/{statusPageId}/password", {
      ...statusPagePath(statusPageId),
      body: { password },
    }),
  );

export const clearStatusPagePassword = (statusPageId: string) =>
  unwrap(
    client.DELETE("/api/v1/status-pages/{statusPageId}/password", statusPagePath(statusPageId)),
  );

export const listStatusPageComponents = (statusPageId: string) =>
  unwrap(
    client.GET("/api/v1/status-pages/{statusPageId}/components", statusPagePath(statusPageId)),
  );

/** Replaces the components: the list is the new content and order of the page. */
export const putStatusPageComponents = (statusPageId: string, items: StatusPageComponentInput[]) =>
  unwrap(
    client.PUT("/api/v1/status-pages/{statusPageId}/components", {
      ...statusPagePath(statusPageId),
      body: { items },
    }),
  );

/** The public view as /status/<slug>.json would serve it now, also while unpublished. */
export const previewStatusPage = (statusPageId: string) =>
  unwrap(client.GET("/api/v1/status-pages/{statusPageId}/preview", statusPagePath(statusPageId)));

// OAuth sign-in (O-01, O-02, ADR-0104).

/** The providers the login page offers and whether the password form is on (no session). */
export const getAuthProviders = () => unwrap(client.GET("/api/v1/auth/providers"));

/**
 * Starts a sign-in or link at a provider and returns the URL to navigate the window to. An
 * invitation token goes in this body only, never in a URL; a link sends the CSRF token (apiFetch).
 */
export const startOAuth = (
  provider: OAuthProvider,
  body: { intent?: "login" | "link"; inviteToken?: string },
) =>
  unwrap(
    client.POST("/api/v1/auth/oauth/{provider}/start", { params: { path: { provider } }, body }),
  );

export const listIdentities = () => unwrap(client.GET("/api/v1/auth/identities"));

export const unlinkIdentity = (provider: OAuthProvider) =>
  unwrap(client.DELETE("/api/v1/auth/identities/{provider}", { params: { path: { provider } } }));

export const listAuthDomains = () => unwrap(client.GET("/api/v1/organization/auth-domains"));

/** Claims a Workspace domain; it always starts with the viewer role (O-04). */
export const createAuthDomain = (domain: string) =>
  unwrap(client.POST("/api/v1/organization/auth-domains", { body: { domain } }));

export const updateAuthDomain = (domainId: string, body: AuthDomainUpdate) =>
  unwrap(
    client.PATCH("/api/v1/organization/auth-domains/{domainId}", {
      params: { path: { domainId } },
      body,
    }),
  );

export const deleteAuthDomain = (domainId: string) =>
  unwrap(
    client.DELETE("/api/v1/organization/auth-domains/{domainId}", {
      params: { path: { domainId } },
    }),
  );
