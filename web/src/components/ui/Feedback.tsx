import {
  cloneElement,
  useEffect,
  useId,
  useRef,
  useState,
  type ReactElement,
  type ReactNode,
} from "react";
import { Button, type ButtonVariant } from "./Button";
import { Skeleton } from "./Data";
import { Input } from "./Field";
import { cx, DiamondShape, DotShape, TriangleShape } from "./shapes";

type AlertTone = "danger" | "warning" | "neutral";

/** A banner in the flow of the page. Danger interrupts a screen reader; the others do not. */
export function Alert({
  tone = "danger",
  title,
  action,
  children,
}: {
  tone?: AlertTone;
  title?: ReactNode;
  action?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div
      role={tone === "danger" ? "alert" : "status"}
      className={cx(
        "flex flex-wrap items-start gap-2.5 border px-3.5 py-3",
        tone === "danger" && "border-down-border bg-down-tint",
        tone === "warning" && "border-warning-border bg-warning-tint",
        tone === "neutral" && "border-border bg-muted",
      )}
    >
      {tone === "danger" && <TriangleShape className="mt-1.5 text-down" />}
      {tone === "warning" && <DiamondShape className="mt-[7px] text-warning" />}
      <div className="min-w-0 flex-1">
        {title && <strong className="font-semibold">{title} </strong>}
        {children}
      </div>
      {action}
    </div>
  );
}

export function EmptyState({
  title,
  action,
  children,
}: {
  title: ReactNode;
  action?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-3.5 border border-border px-6 py-7">
      <div aria-hidden="true" className="h-[18px] border-b border-dashed border-input" />
      <h2 className="text-[20px] font-semibold tracking-[-0.01em]">{title}</h2>
      {children && <p className="max-w-[340px] text-muted-foreground">{children}</p>}
      {action && <div className="flex gap-2">{action}</div>}
    </div>
  );
}

/** `section` keeps the page's layout when only one part of it failed to load. */
export function ErrorState({
  title,
  detail,
  meta,
  onRetry,
  variant = "page",
}: {
  title: ReactNode;
  detail?: ReactNode;
  /** The HTTP status and request id, for a support question. */
  meta?: ReactNode;
  onRetry?: () => void;
  variant?: "page" | "section";
}) {
  if (variant === "section") {
    return (
      <div
        role="alert"
        className="flex items-center gap-2.5 border border-border-faint px-3 py-2.5"
      >
        <TriangleShape size="small" className="text-down" />
        <span className="flex-1 text-muted-foreground">{title}</span>
        {onRetry && (
          <Button variant="link" onClick={onRetry}>
            Retry
          </Button>
        )}
      </div>
    );
  }
  return (
    <div role="alert" className="flex flex-col gap-3 border border-border px-6 py-7">
      <h2 className="flex items-center gap-2 text-[17px] font-semibold">
        <TriangleShape className="text-down" />
        {title}
      </h2>
      {detail && <p className="text-muted-foreground">{detail}</p>}
      {meta && <p className="font-mono text-mono text-subtle-foreground">{meta}</p>}
      {onRetry && (
        <div>
          <Button variant="secondary" size="small" onClick={onRetry}>
            Retry
          </Button>
        </div>
      )}
    </div>
  );
}

/** Save results. Presentational: timing and the live region belong to the ToastProvider. */
export function Toast({
  kind = "success",
  action,
  onDismiss,
  children,
}: {
  kind?: "success" | "error";
  action?: ReactNode;
  onDismiss?: () => void;
  children: ReactNode;
}) {
  return (
    <div
      className={cx(
        "flex items-center gap-2.5 border bg-popover py-2.5 pr-2 pl-3.5 shadow-overlay",
        kind === "error" ? "border-down-border" : "border-border",
      )}
    >
      {kind === "error" ? (
        <TriangleShape className="text-down" />
      ) : (
        <DotShape size="large" className="text-success" />
      )}
      <span className="min-w-0 flex-1 font-medium">{children}</span>
      {action}
      {onDismiss && (
        <button
          type="button"
          aria-label="Dismiss"
          className="size-6 cursor-pointer text-[16px] text-muted-foreground hover:bg-muted hover:text-foreground"
          onClick={onDismiss}
        >
          ×
        </button>
      )}
    </div>
  );
}

/** Opens on hover and focus after 300 ms. A reason that must be read belongs next to the control. */
export function Tooltip({
  content,
  children,
}: {
  content: ReactNode;
  children: ReactElement<{ "aria-describedby"?: string }>;
}) {
  const id = useId();
  return (
    <span className="group relative inline-flex">
      {cloneElement(children, {
        // Adds to the child's own description instead of replacing it.
        "aria-describedby": [children.props["aria-describedby"], id].filter(Boolean).join(" "),
      })}
      <span
        id={id}
        role="tooltip"
        className="pointer-events-none absolute bottom-full left-0 z-10 mb-1.5 w-max max-w-64 bg-inverse px-2 py-1.5 text-caption text-inverse-foreground opacity-0 transition-opacity group-focus-within:opacity-100 group-focus-within:delay-300 group-hover:opacity-100 group-hover:delay-300"
      >
        {content}
      </span>
    </span>
  );
}

/**
 * A secret shown only once. Reports a failed copy honestly: the button never says "Copied" unless
 * the clipboard accepted the value.
 */
export function CopyField({
  label,
  value,
  copyLabel = "Copy",
  once = true,
}: {
  label: string;
  value: string;
  copyLabel?: string;
  once?: boolean;
}) {
  const id = useId();
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  async function copy() {
    clearTimeout(timer.current);
    try {
      await navigator.clipboard.writeText(value);
      setState("copied");
      timer.current = setTimeout(() => setState("idle"), 2000);
    } catch {
      setState("failed");
    }
  }

  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={id} className="text-caption font-medium">
        {label}
      </label>
      <div className="flex flex-col gap-2 sm:flex-row sm:gap-0">
        <Input
          id={id}
          mono
          readOnly
          value={value}
          aria-describedby={once ? `${id}-once` : undefined}
          onFocus={(e) => e.currentTarget.select()}
        />
        <Button className="min-w-[88px] flex-none" onClick={() => void copy()}>
          {state === "copied" ? "Copied ✓" : copyLabel}
        </Button>
      </div>
      {state === "failed" && (
        <span role="alert" className="flex items-start gap-1.5 text-caption text-down">
          <TriangleShape size="small" className="mt-[5px]" />
          Copying failed. Select the value and copy it by hand.
        </span>
      )}
      {once && (
        <span
          id={`${id}-once`}
          className="flex items-center gap-1.5 border border-warning-border bg-warning-tint px-2.5 py-2 text-caption"
        >
          <DiamondShape className="text-warning" />
          Copy it now: it is shown only once.
        </span>
      )}
    </div>
  );
}

/** A stored secret that can be replaced but never read back. */
export function WriteOnlyValue({
  label,
  hint,
  actionLabel = "Replace",
  onReplace,
}: {
  label: string;
  hint?: ReactNode;
  actionLabel?: string;
  onReplace: () => void;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-caption font-medium">{label}</span>
      <div className="flex items-stretch">
        <div className="box-border flex h-11 flex-1 items-center gap-2 border border-dashed border-input bg-muted px-2.5 text-muted-foreground sm:h-9">
          <span aria-hidden="true" className="font-mono tracking-[0.1em]">
            ••••
          </span>
          Configured, hidden
        </div>
        <Button variant="secondary" className="border-l-0" onClick={onReplace}>
          {actionLabel}
        </Button>
      </div>
      {hint && <span className="text-caption text-muted-foreground">{hint}</span>}
    </div>
  );
}

/**
 * A modal confirmation on the native dialog: the browser traps focus and makes the page inert.
 * Focus starts on Cancel and Escape cancels, so a stray Enter can never confirm.
 */
export function ConfirmDialog({
  title,
  children,
  confirmLabel,
  confirmVariant = "destructive",
  cancelLabel = "Cancel",
  busy = false,
  onConfirm,
  onCancel,
}: {
  title: ReactNode;
  children: ReactNode;
  confirmLabel: ReactNode;
  confirmVariant?: ButtonVariant;
  cancelLabel?: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const id = useId();
  const dialog = useRef<HTMLDialogElement>(null);
  const cancel = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    element.showModal();
    cancel.current?.focus();
    return () => element.close();
  }, []);

  return (
    <dialog
      ref={dialog}
      aria-labelledby={`${id}-title`}
      aria-describedby={`${id}-desc`}
      className="m-auto w-[calc(100%-32px)] max-w-[420px] border border-border bg-popover text-foreground shadow-overlay backdrop:bg-overlay"
      onKeyDown={(e) => {
        // Escape is handled here, before the browser turns it into a cancel event: the same
        // in every browser and in jsdom, which has no native dialog behaviour.
        if (e.key !== "Escape") return;
        e.preventDefault();
        if (!busy) onCancel();
      }}
      onCancel={(e) => {
        // Other close requests (a phone's back gesture). The owner closes it by unmounting, so
        // its state cannot disagree with the browser's.
        e.preventDefault();
        if (!busy) onCancel();
      }}
    >
      <div className="flex flex-col gap-2.5 p-5 pb-4">
        <h2 id={`${id}-title`} className="text-title font-semibold tracking-[-0.01em]">
          {title}
        </h2>
        <div id={`${id}-desc`} className="flex flex-col gap-2.5 text-muted-foreground">
          {children}
        </div>
      </div>
      <div className="flex justify-end gap-2 border-t border-border-faint px-5 py-3">
        <Button ref={cancel} variant="secondary" disabled={busy} onClick={onCancel}>
          {cancelLabel}
        </Button>
        <Button variant={confirmVariant} busy={busy} onClick={onConfirm}>
          {confirmLabel}
        </Button>
      </div>
    </dialog>
  );
}

/** A section is loading: static shapes for the eye, the label for screen readers. */
export function Loading({
  label,
  className,
  children,
}: {
  label: string;
  className?: string;
  /** The skeleton; one bar when left out. */
  children?: ReactNode;
}) {
  return (
    // The default gap only when the caller sets none: utilities are not merged.
    <div
      aria-busy="true"
      className={cx("flex flex-col", !/(^|\s)gap-/.test(className ?? "") && "gap-3", className)}
    >
      {children ?? <Skeleton className="w-1/2" />}
      <span className="sr-only">{label}</span>
    </div>
  );
}

/** A quiet one-line empty result: "No incidents.", "No tenants match …". */
export function EmptyNote({ children }: { children: ReactNode }) {
  return <p className="border border-border-faint px-4 py-3 text-muted-foreground">{children}</p>;
}
