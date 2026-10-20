import { Link } from "react-router-dom";

export default function MonitorNotFound() {
  return (
    <section>
      <h1>Monitor not found</h1>
      <p>It may have been deleted.</p>
      <Link to="/">Go to monitors</Link>
    </section>
  );
}
