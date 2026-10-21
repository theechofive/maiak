import { Outlet } from "react-router-dom";
import NotFound from "../pages/NotFound";
import { useAuth } from "./AuthProvider";

/**
 * Renders the /admin routes only for the platform admin (ADR-0097). Everybody else gets the
 * not-found page, so the area is not even revealed; the API refuses them with 403 anyway.
 */
export default function RequirePlatformAdmin() {
  const auth = useAuth();
  if (auth.session?.user.isPlatformAdmin !== true) return <NotFound />;
  return <Outlet />;
}
