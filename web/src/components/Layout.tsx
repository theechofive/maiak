import { useState } from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useAuth } from "../auth/AuthProvider";
import ErrorBoundary from "./ErrorBoundary";
import OrganizationSwitcher from "./OrganizationSwitcher";
import ThemeToggle from "./ThemeToggle";
import { Badge, Button, cx } from "./ui";

/** The page width shared by the top bar and the content, so their edges line up. */
export const PAGE = "mx-auto w-full max-w-[1200px] px-4 sm:px-8";

const navClass = ({ isActive }: { isActive: boolean }) =>
  cx(
    "flex items-center",
    isActive
      ? "font-medium shadow-[inset_0_-2px_0_var(--foreground)]"
      : "text-muted-foreground hover:text-foreground",
  );

export default function Layout() {
  const auth = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  // Keyed by path below, so the phone menu closes on every navigation.
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const menuOpen = menuFor === location.pathname;
  const login = auth.session?.user.login;
  const isAdmin = auth.session?.user.isPlatformAdmin === true;
  // The grey band with its own navigation is the signal that this is the privileged area.
  const inAdmin = isAdmin && /^\/admin(\/|$)/.test(location.pathname);

  async function onLogout() {
    await auth.logout();
    navigate("/login", { replace: true });
  }

  const links = (
    <>
      <NavLink to="/" end className={navClass}>
        Monitors
      </NavLink>
      <NavLink to="/members" className={navClass}>
        Members
      </NavLink>
      <NavLink to="/settings" className={navClass}>
        Settings
      </NavLink>
      {isAdmin && (
        <NavLink to="/admin" className={navClass}>
          Admin
        </NavLink>
      )}
    </>
  );

  return (
    <div className="flex min-h-dvh flex-col">
      <header className="border-b border-border">
        <div className={cx(PAGE, "flex h-[52px] items-stretch justify-between")}>
          <div className="flex items-stretch gap-8">
            <Link
              to="/"
              className="flex items-center font-mono text-caption font-semibold tracking-[0.18em]"
            >
              MAIAK
            </Link>
            <nav aria-label="Main" className="flex items-stretch gap-[22px] max-sm:hidden">
              {links}
            </nav>
          </div>
          <div className="flex items-center gap-3.5 max-sm:hidden">
            <OrganizationSwitcher />
            <span className="font-mono text-mono text-muted-foreground">{login}</span>
            <ThemeToggle />
            <Button variant="ghost" size="small" onClick={() => void onLogout()}>
              Log out
            </Button>
          </div>
          <Button
            variant="ghost"
            aria-expanded={menuOpen}
            aria-controls="phone-menu"
            className="-mr-2 self-center sm:hidden"
            onClick={() => setMenuFor(menuOpen ? null : location.pathname)}
          >
            <span aria-hidden="true" className="flex w-3.5 flex-col gap-[3px]">
              <span className="h-[1.5px] bg-current" />
              <span className="h-[1.5px] bg-current" />
              <span className="h-[1.5px] bg-current" />
            </span>
            Menu
          </Button>
        </div>
        {menuOpen && (
          // eslint-disable-next-line jsx-a11y/no-static-element-interactions -- Escape closes the menu from any of its links; it is not a control itself.
          <div
            id="phone-menu"
            className="flex flex-col border-t border-border-faint px-4 pb-3 sm:hidden"
            onKeyDown={(e) => {
              if (e.key === "Escape") setMenuFor(null);
            }}
          >
            <nav aria-label="Main" className="flex flex-col [&>a]:h-11 [&>a]:shadow-none">
              {links}
            </nav>
            <div className="flex items-center justify-between gap-3 border-t border-border-faint py-3">
              <OrganizationSwitcher className="max-w-none flex-1" />
            </div>
            <div className="flex items-center justify-between gap-3 border-t border-border-faint pt-3">
              <span className="font-mono text-mono text-muted-foreground">{login}</span>
              <span className="flex items-center gap-2">
                <ThemeToggle />
                <Button variant="secondary" size="small" onClick={() => void onLogout()}>
                  Log out
                </Button>
              </span>
            </div>
          </div>
        )}
      </header>
      {inAdmin && (
        <div className="border-b border-border bg-muted">
          <div className={cx(PAGE, "flex h-11 items-stretch gap-6")}>
            <span className="flex items-center">
              <Badge tone="inverse">Platform admin</Badge>
            </span>
            <nav aria-label="Admin" className="flex items-stretch gap-5">
              <NavLink to="/admin" end className={navClass}>
                Overview
              </NavLink>
              <NavLink to="/admin/tenants" className={navClass}>
                Tenants
              </NavLink>
            </nav>
          </div>
        </div>
      )}
      <main className={cx(PAGE, "flex flex-1 flex-col pb-16")}>
        {/* Keyed by path so an error on one page does not stick after navigating away. */}
        <ErrorBoundary key={location.pathname}>
          <Outlet />
        </ErrorBoundary>
      </main>
    </div>
  );
}
