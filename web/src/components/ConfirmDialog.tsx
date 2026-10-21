import type { ReactNode } from "react";

/**
 * A confirmation in the style of the monitor delete dialog: non-modal `<dialog open>` with focus
 * on Cancel and Escape to close (L-051).
 */
export default function ConfirmDialog({
  id,
  title,
  children,
  confirmLabel,
  busy,
  onConfirm,
  onCancel,
}: {
  id: string;
  title: string;
  children: ReactNode;
  confirmLabel: string;
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <dialog
      open
      aria-labelledby={`${id}-title`}
      aria-describedby={`${id}-desc`}
      onKeyDown={(e) => {
        if (e.key === "Escape") onCancel();
      }}
    >
      <article>
        <h2 id={`${id}-title`}>{title}</h2>
        <div id={`${id}-desc`}>{children}</div>
        <footer className="actions">
          <button type="button" className="secondary" autoFocus onClick={onCancel}>
            Cancel
          </button>
          <button type="button" aria-busy={busy} disabled={busy} onClick={onConfirm}>
            {confirmLabel}
          </button>
        </footer>
      </article>
    </dialog>
  );
}
