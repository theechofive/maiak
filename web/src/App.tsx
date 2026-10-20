import { Route, Routes } from "react-router-dom";
import Login from "./pages/Login";
import MonitorDetails from "./pages/MonitorDetails";
import MonitorForm from "./pages/MonitorForm";
import Monitors from "./pages/Monitors";
import Settings from "./pages/Settings";

export default function App() {
  return (
    <main className="container">
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="/" element={<Monitors />} />
        <Route path="/monitors/new" element={<MonitorForm mode="create" />} />
        <Route path="/monitors/:id" element={<MonitorDetails />} />
        <Route path="/monitors/:id/edit" element={<MonitorForm mode="edit" />} />
        <Route path="/settings" element={<Settings />} />
      </Routes>
    </main>
  );
}
