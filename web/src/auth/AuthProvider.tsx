import { QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useMemo, useRef, type ReactNode } from "react";
import { setCsrfToken, setUnauthorizedHandler } from "../api/client";
import * as api from "../api/endpoints";
import { queryKeys, type SessionInfo } from "../api/endpoints";

export interface AuthValue {
  /** "loading" until GET /auth/me answers; "error" when it failed for another reason than 401. */
  status: "loading" | "error" | "ready";
  session: SessionInfo | null;
  error: unknown;
  retry: () => void;
  login: (login: string, password: string) => Promise<void>;
  /**
   * Registers and signs in (the answer is a session like login's). With an invitation token the
   * account joins the inviting organization instead of getting its own.
   */
  register: (email: string, password: string, inviteToken?: string) => Promise<void>;
  logout: () => Promise<void>;
  /** Moves the session to another organization; every cached answer of the old one is dropped. */
  switchOrganization: (tenantId: string) => Promise<void>;
  /** Joins an organization with an invitation; the session moves there. */
  acceptInvitation: (token: string) => Promise<void>;
  /** Forgets a session the server already ended (after leaving or deleting the organization). */
  endSession: () => void;
}

const AuthContext = createContext<AuthValue | null>(null);

/** Forgets the session and every cached response of the previous user. */
function dropSession(queryClient: QueryClient) {
  setCsrfToken(null);
  queryClient.setQueryData(queryKeys.me, null);
  queryClient.removeQueries({ predicate: (q) => q.queryKey[0] !== queryKeys.me[0] });
}

/**
 * Takes a new session (another organization): nothing cached for the old one may show. Reset, not
 * remove: a page that stays mounted would keep showing the removed data; reset puts its queries
 * back to loading and refetches them with the new session.
 */
function replaceSession(queryClient: QueryClient, session: SessionInfo) {
  queryClient.setQueryData(queryKeys.me, session);
  // A used invitation must not be looked up again (a failed lookup counts against the limit).
  queryClient.removeQueries({ queryKey: [queryKeys.invitationPreview("")[0]] });
  void queryClient.resetQueries({ predicate: (q) => q.queryKey[0] !== queryKeys.me[0] });
}

/**
 * Tabs of the app tell each other when the session changed (login, logout, organization switch,
 * invitation accepted), so no tab keeps showing another organization's data or a dead session.
 * The message carries no token or id: the other tabs ask the server again.
 */
export const SESSION_CHANNEL = "maiak-session";

function openSessionChannel(): BroadcastChannel | null {
  return typeof BroadcastChannel === "undefined" ? null : new BroadcastChannel(SESSION_CHANNEL);
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const me = useQuery({
    queryKey: queryKeys.me,
    queryFn: api.getMe,
    retry: false,
    staleTime: Infinity,
    refetchInterval: false,
    refetchOnWindowFocus: false,
  });

  // Any 401 ends the session in the UI; the route guard then redirects to /login with the target path.
  useEffect(() => setUnauthorizedHandler(() => dropSession(queryClient)), [queryClient]);

  const channel = useRef<BroadcastChannel | null>(null);
  useEffect(() => {
    const ch = openSessionChannel();
    channel.current = ch;
    if (!ch) return;
    ch.onmessage = () => {
      void queryClient.resetQueries({ predicate: (q) => q.queryKey[0] !== queryKeys.me[0] });
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    };
    return () => {
      ch.close();
      channel.current = null;
    };
  }, [queryClient]);

  const value = useMemo<AuthValue>(() => {
    const announce = () => channel.current?.postMessage({ type: "session-changed" });
    return {
      status: me.isPending ? "loading" : me.isError ? "error" : "ready",
      session: me.data ?? null,
      error: me.error,
      retry: () => void me.refetch(),
      login: async (login, password) => {
        const session = await api.login(login, password);
        queryClient.setQueryData(queryKeys.me, session);
        announce();
      },
      register: async (email, password, inviteToken) => {
        const session = await api.register(email, password, inviteToken);
        queryClient.setQueryData(queryKeys.me, session);
        announce();
      },
      switchOrganization: async (tenantId) => {
        replaceSession(queryClient, await api.switchOrganization(tenantId));
        announce();
      },
      acceptInvitation: async (token) => {
        replaceSession(queryClient, await api.acceptInvitation(token));
        announce();
      },
      endSession: () => {
        dropSession(queryClient);
        announce();
      },
      logout: async () => {
        try {
          await api.logout();
        } catch {
          // The session is dropped locally even when the server call fails.
        }
        dropSession(queryClient);
        announce();
      },
    };
  }, [me, queryClient]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth must be used inside AuthProvider");
  return value;
}
