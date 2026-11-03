import { Link } from "react-router-dom";
import { buttonClass } from "../components/ui";

export default function NotFound() {
  return (
    <section className="flex flex-col items-start gap-4 py-10">
      <h1 className="text-display font-semibold tracking-[-0.02em] max-sm:text-headline">
        Page not found
      </h1>
      <Link to="/" className={buttonClass("secondary")}>
        Go to monitors
      </Link>
    </section>
  );
}
