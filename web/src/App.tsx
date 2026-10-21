import { Route, Routes } from "react-router-dom";
import RequireAuth from "./auth/RequireAuth";
import Layout from "./components/Layout";
import Login from "./pages/Login";
import MonitorDetails from "./pages/MonitorDetails";
import MonitorForm from "./pages/MonitorForm";
import Monitors from "./pages/Monitors";
import NotFound from "./pages/NotFound";
import Register from "./pages/Register";
import Settings from "./pages/Settings";

export default function App() {
  return (
    <Routes>
      <Route
        path="/login"
        element={
          <main className="container">
            <Login />
          </main>
        }
      />
      <Route
        path="/register"
        element={
          <main className="container">
            <Register />
          </main>
        }
      />
      <Route element={<RequireAuth />}>
        <Route element={<Layout />}>
          <Route path="/" element={<Monitors />} />
          <Route path="/monitors/new" element={<MonitorForm mode="create" />} />
          <Route path="/monitors/:id" element={<MonitorDetails />} />
          <Route path="/monitors/:id/edit" element={<MonitorForm mode="edit" />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="*" element={<NotFound />} />
        </Route>
      </Route>
    </Routes>
  );
}
