import { Navigate, Outlet, useLocation } from "react-router-dom";
import { ErrorState, Loading } from "../components/QueryState";
import { useAuth } from "./AuthProvider";

/** Renders the child routes only with a session; otherwise redirects to /login keeping the target. */
export default function RequireAuth() {
  const auth = useAuth();
  const location = useLocation();

  if (auth.status === "loading") {
    return (
      <main className="container">
        <Loading label="Checking your session…" />
      </main>
    );
  }
  if (auth.status === "error") {
    return (
      <main className="container">
        <ErrorState title="Cannot load your session" error={auth.error} onRetry={auth.retry} />
      </main>
    );
  }
  if (!auth.session) {
    const from = location.pathname + location.search + location.hash;
    return <Navigate to="/login" replace state={{ from }} />;
  }
  return <Outlet />;
}

/** The path to return to after login: only same-app paths, never back to /login or /register. */
export function redirectTarget(state: unknown): string {
  const from =
    typeof state === "object" && state !== null ? (state as { from?: unknown }).from : undefined;
  if (
    typeof from !== "string" ||
    !from.startsWith("/") ||
    from.startsWith("//") ||
    from.includes("\\")
  ) {
    return "/";
  }
  for (const page of ["/login", "/register"]) {
    if (from === page || from.startsWith(`${page}?`)) return "/";
  }
  return from;
}
