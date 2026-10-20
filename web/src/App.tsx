import { Outlet, Route, Routes, useNavigate } from "react-router-dom";
import { useAuth } from "./auth/AuthProvider";
import RequireAuth from "./auth/RequireAuth";
import Login from "./pages/Login";
import MonitorDetails from "./pages/MonitorDetails";
import MonitorForm from "./pages/MonitorForm";
import Monitors from "./pages/Monitors";
import Settings from "./pages/Settings";

function SignedInLayout() {
  const auth = useAuth();
  const navigate = useNavigate();
  return (
    <>
      <nav>
        <span>{auth.session?.user.email}</span>
        <button
          type="button"
          onClick={async () => {
            await auth.logout();
            navigate("/login", { replace: true });
          }}
        >
          Log out
        </button>
      </nav>
      <Outlet />
    </>
  );
}

export default function App() {
  return (
    <main className="container">
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route element={<RequireAuth />}>
          <Route element={<SignedInLayout />}>
            <Route path="/" element={<Monitors />} />
            <Route path="/monitors/new" element={<MonitorForm mode="create" />} />
            <Route path="/monitors/:id" element={<MonitorDetails />} />
            <Route path="/monitors/:id/edit" element={<MonitorForm mode="edit" />} />
            <Route path="/settings" element={<Settings />} />
          </Route>
        </Route>
      </Routes>
    </main>
  );
}
