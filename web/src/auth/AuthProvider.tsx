import { QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useMemo, type ReactNode } from "react";
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

  const value = useMemo<AuthValue>(
    () => ({
      status: me.isPending ? "loading" : me.isError ? "error" : "ready",
      session: me.data ?? null,
      error: me.error,
      retry: () => void me.refetch(),
      login: async (login, password) => {
        const session = await api.login(login, password);
        queryClient.setQueryData(queryKeys.me, session);
      },
      register: async (email, password, inviteToken) => {
        const session = await api.register(email, password, inviteToken);
        queryClient.setQueryData(queryKeys.me, session);
      },
      switchOrganization: async (tenantId) => {
        replaceSession(queryClient, await api.switchOrganization(tenantId));
      },
      acceptInvitation: async (token) => {
        replaceSession(queryClient, await api.acceptInvitation(token));
      },
      endSession: () => dropSession(queryClient),
      logout: async () => {
        try {
          await api.logout();
        } catch {
          // The session is dropped locally even when the server call fails.
        }
        dropSession(queryClient);
      },
    }),
    [me, queryClient],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth must be used inside AuthProvider");
  return value;
}
