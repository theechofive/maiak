import { errorMessage } from "../api/client";

export function Loading({ label = "Loading…" }: { label?: string }) {
  return <p aria-busy="true">{label}</p>;
}

export function ErrorState({
  title,
  error,
  onRetry,
}: {
  title: string;
  error: unknown;
  onRetry?: () => void;
}) {
  return (
    <article role="alert" className="error-state">
      <strong>{title}</strong>
      <p>{errorMessage(error)}</p>
      {onRetry && (
        <button type="button" className="secondary" onClick={onRetry}>
          Try again
        </button>
      )}
    </article>
  );
}
