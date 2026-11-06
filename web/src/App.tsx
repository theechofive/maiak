import { Route, Routes } from "react-router-dom";
import RequireAuth from "./auth/RequireAuth";
import RequirePlatformAdmin from "./auth/RequirePlatformAdmin";
import Layout from "./components/Layout";
import Invite from "./pages/Invite";
import Login from "./pages/Login";
import Members from "./pages/Members";
import MonitorDetails from "./pages/MonitorDetails";
import MonitorForm from "./pages/MonitorForm";
import Monitors from "./pages/Monitors";
import NotFound from "./pages/NotFound";
import Profile from "./pages/Profile";
import Projects from "./pages/Projects";
import Register from "./pages/Register";
import Settings from "./pages/Settings";
import StatusPageSettings from "./pages/StatusPage";
import AdminOverview from "./pages/admin/AdminOverview";
import AdminTenantDetails from "./pages/admin/AdminTenantDetails";
import AdminTenants from "./pages/admin/AdminTenants";

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/register" element={<Register />} />
      <Route path="/invite" element={<Invite />} />
      <Route element={<RequireAuth />}>
        <Route element={<Layout />}>
          <Route path="/" element={<Monitors />} />
          <Route path="/monitors/new" element={<MonitorForm mode="create" />} />
          <Route path="/monitors/:id" element={<MonitorDetails />} />
          <Route path="/monitors/:id/edit" element={<MonitorForm mode="edit" />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="/profile" element={<Profile />} />
          <Route path="/members" element={<Members />} />
          <Route path="/projects" element={<Projects />} />
          <Route path="/status-page" element={<StatusPageSettings />} />
          <Route path="/admin" element={<RequirePlatformAdmin />}>
            <Route index element={<AdminOverview />} />
            <Route path="tenants" element={<AdminTenants />} />
            <Route path="tenants/:id" element={<AdminTenantDetails />} />
            <Route path="*" element={<NotFound />} />
          </Route>
          <Route path="*" element={<NotFound />} />
        </Route>
      </Route>
    </Routes>
  );
}
