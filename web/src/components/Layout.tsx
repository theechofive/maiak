import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useAuth } from "../auth/AuthProvider";
import ErrorBoundary from "./ErrorBoundary";

export default function Layout() {
  const auth = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  async function onLogout() {
    await auth.logout();
    navigate("/login", { replace: true });
  }

  return (
    <>
      <header className="container">
        <nav aria-label="Main">
          <ul>
            <li>
              <Link to="/" className="brand">
                <strong>Maiak</strong>
              </Link>
            </li>
            <li>
              <NavLink to="/" end>
                Monitors
              </NavLink>
            </li>
            <li>
              <NavLink to="/settings">Settings</NavLink>
            </li>
          </ul>
          <ul>
            <li>
              <small>{auth.session?.user.email}</small>
            </li>
            <li>
              <button type="button" className="outline secondary" onClick={onLogout}>
                Log out
              </button>
            </li>
          </ul>
        </nav>
      </header>
      <main className="container">
        {/* Keyed by path so an error on one page does not stick after navigating away. */}
        <ErrorBoundary key={location.pathname}>
          <Outlet />
        </ErrorBoundary>
      </main>
    </>
  );
}
