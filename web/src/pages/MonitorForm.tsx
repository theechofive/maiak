import Placeholder from "./Placeholder";

export default function MonitorForm({ mode }: { mode: "create" | "edit" }) {
  return <Placeholder title={mode === "create" ? "Add monitor" : "Edit monitor"} task="F-06" />;
}
