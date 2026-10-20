import { Navigate, Outlet, useLocation } from "react-router-dom";
import { errorMessage } from "../api/client";
import { useAuth } from "./AuthProvider";

/** Renders the child routes only with a session; otherwise redirects to /login keeping the target. */
export default function RequireAuth() {
  const auth = useAuth();
  const location = useLocation();

  if (auth.status === "loading") {
    return <p aria-busy="true">Checking your session…</p>;
  }
  if (auth.status === "error") {
    return (
      <section role="alert">
        <h1>Cannot load your session</h1>
        <p>{errorMessage(auth.error)}</p>
        <button type="button" onClick={auth.retry}>
          Try again
        </button>
      </section>
    );
  }
  if (!auth.session) {
    const from = location.pathname + location.search + location.hash;
    return <Navigate to="/login" replace state={{ from }} />;
  }
  return <Outlet />;
}

/** The path to return to after login: only same-app paths, never back to /login. */
export function redirectTarget(state: unknown): string {
  const from =
    typeof state === "object" && state !== null ? (state as { from?: unknown }).from : undefined;
  if (typeof from !== "string" || !from.startsWith("/") || from.startsWith("//")) return "/";
  if (from === "/login" || from.startsWith("/login?")) return "/";
  return from;
}
