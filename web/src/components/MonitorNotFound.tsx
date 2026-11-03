import { Link } from "react-router-dom";
import { buttonClass } from "./ui";

export default function MonitorNotFound() {
  return (
    <section className="flex flex-col items-start gap-3 py-10">
      <h1 className="text-headline font-semibold tracking-[-0.02em] sm:text-display">
        Monitor not found
      </h1>
      <p className="text-muted-foreground">It may have been deleted.</p>
      <Link to="/" className={buttonClass("secondary")}>
        Go to monitors
      </Link>
    </section>
  );
}
