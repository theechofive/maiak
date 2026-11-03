import {
  useId,
  type ComponentProps,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";
import { cx, TriangleShape } from "./shapes";

export interface ControlProps {
  id: string;
  "aria-invalid"?: true;
  "aria-describedby"?: string;
}

/**
 * A label above and a hint or an error below, wired to the control by id:
 * `<Field label="Name" error={e}>{(c) => <Input {...c} value={v} />}</Field>`.
 */
export function Field({
  label,
  hint,
  error,
  className,
  children,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  className?: string;
  children: (control: ControlProps) => ReactNode;
}) {
  const id = useId();
  const messageId = `${id}-message`;
  const message = error ?? hint;
  return (
    <div className={cx("flex min-w-0 flex-col gap-1.5", className)}>
      <label htmlFor={id} className="text-caption font-medium">
        {label}
      </label>
      {children({
        id,
        ...(error ? { "aria-invalid": true as const } : {}),
        ...(message ? { "aria-describedby": messageId } : {}),
      })}
      {error ? (
        <span id={messageId} className="flex items-start gap-1.5 text-caption text-down">
          <TriangleShape size="small" className="mt-[5px]" />
          <span>{error}</span>
        </span>
      ) : hint ? (
        <span id={messageId} className="text-caption text-muted-foreground">
          {hint}
        </span>
      ) : null}
    </div>
  );
}

// 16 px text on phones: iOS zooms the page into any smaller input.
// No horizontal padding here: utilities are not merged, so each use adds exactly the padding it needs.
const CONTROL =
  "box-border h-11 w-full min-w-0 border border-input bg-field text-[16px] text-foreground transition-colors outline-offset-1 hover:border-input-hover disabled:cursor-not-allowed disabled:border-border-faint disabled:bg-muted disabled:text-subtle-foreground aria-invalid:border-destructive sm:h-9 sm:text-body";

export function Input({
  mono = false,
  secret = false,
  start,
  end,
  action,
  className,
  ...props
}: ComponentProps<"input"> & {
  /** URLs, numbers, tokens. */
  mono?: boolean;
  /** Masks the text without inviting a password manager. */
  secret?: boolean;
  /** Fixed text inside the field, such as "https://" or a unit. */
  start?: ReactNode;
  end?: ReactNode;
  /** A small button inside the field, such as Show for a password. Unlike `end`, it is focusable. */
  action?: ReactNode;
}) {
  const text = cx(mono && "font-mono sm:text-caption", secret && "secret-text");
  if (!start && !end && !action)
    return <input className={cx(CONTROL, "px-2.5", text, className)} {...props} />;
  return (
    <div
      className={cx(
        CONTROL,
        "flex items-stretch focus-within:outline-2 focus-within:outline-ring has-aria-invalid:border-destructive",
        className,
      )}
    >
      {start && <Affix className="pl-2.5">{start}</Affix>}
      <input
        className={cx(
          "min-w-0 flex-1 bg-transparent outline-none",
          start ? "pl-px" : "pl-2.5",
          end || action ? "pr-1" : "pr-2.5",
          text,
        )}
        {...props}
      />
      {end && <Affix className="pr-2.5">{end}</Affix>}
      {action && <span className="flex items-center pr-1">{action}</span>}
    </div>
  );
}

function Affix({ className, children }: { className: string; children: ReactNode }) {
  return (
    <span
      aria-hidden="true"
      className={cx("flex items-center font-mono text-caption text-muted-foreground", className)}
    >
      {children}
    </span>
  );
}

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cx(CONTROL, "h-auto px-2.5 py-2 sm:h-auto", className)} {...props} />;
}

/** Native on purpose: the platform's list works on every phone and with every screen reader. */
export function Select({ className, children, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <span className={cx("relative block", className)}>
      <select className={cx(CONTROL, "cursor-pointer appearance-none pr-7 pl-2.5")} {...props}>
        {children}
      </select>
      <span
        aria-hidden="true"
        className="pointer-events-none absolute top-1/2 right-3 size-1.5 -translate-y-[70%] rotate-45 border-r-[1.5px] border-b-[1.5px] border-muted-foreground"
      />
    </span>
  );
}

/** A value the user can see but not change yet: a dashed box, not a disabled input. */
export function ReadOnlyValue({ children, note }: { children: ReactNode; note?: ReactNode }) {
  return (
    <div className="box-border flex h-11 items-center justify-between gap-3 border border-dashed border-input bg-muted px-2.5 font-mono text-caption sm:h-9">
      <span>{children}</span>
      {note && <span className="font-sans text-muted-foreground">{note}</span>}
    </div>
  );
}

export function Checkbox({
  label,
  description,
  error,
  className,
  id: idProp,
  ...props
}: Omit<InputHTMLAttributes<HTMLInputElement>, "type"> & {
  label: ReactNode;
  description?: ReactNode;
  /** A server's answer about this choice; shown under the description. */
  error?: ReactNode;
}) {
  const ownId = useId();
  const id = idProp ?? ownId;
  // The description is outside the label: it describes the box instead of renaming it.
  return (
    <div
      className={cx(
        "flex items-start gap-2.5 py-2.5 has-disabled:text-subtle-foreground sm:py-0",
        className,
      )}
    >
      <span className="relative mt-0.5 flex size-4 flex-none">
        <input
          id={id}
          type="checkbox"
          aria-invalid={error ? true : undefined}
          aria-describedby={
            [error && `${id}-error`, description && `${id}-description`]
              .filter(Boolean)
              .join(" ") || undefined
          }
          className="peer absolute inset-0 m-0 cursor-pointer appearance-none border border-input bg-field checked:border-primary checked:bg-primary disabled:cursor-not-allowed disabled:border-border-faint disabled:bg-muted"
          {...props}
        />
        <span
          aria-hidden="true"
          className="pointer-events-none absolute top-[2.5px] left-[5.5px] hidden h-2 w-1 rotate-45 border-r-2 border-b-2 border-primary-foreground peer-checked:block"
        />
      </span>
      <span className="flex flex-col gap-0.5">
        <label htmlFor={id} className="cursor-pointer font-medium">
          {label}
        </label>
        {description && (
          <span id={`${id}-description`} className="text-caption text-muted-foreground">
            {description}
          </span>
        )}
        {error && (
          <span id={`${id}-error`} className="flex items-start gap-1.5 text-caption text-down">
            <TriangleShape size="small" className="mt-[5px]" />
            <span>{error}</span>
          </span>
        )}
      </span>
    </div>
  );
}

/** A few exclusive options in one bar. Native radios give it arrow keys and screen reader names. */
export function Segmented<T extends string>({
  label,
  name,
  value,
  options,
  onChange,
  size = "default",
}: {
  label: string;
  name: string;
  value: T;
  options: { value: T; label: ReactNode; disabled?: boolean }[];
  onChange: (value: T) => void;
  size?: "default" | "small";
}) {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className={cx(
        "inline-flex box-border border border-input",
        size === "default" ? "h-11 sm:h-9" : "h-9 sm:h-7",
      )}
    >
      {options.map((option, i) => (
        <label
          key={option.value}
          className={cx(
            "flex cursor-pointer items-center px-3.5 font-medium transition-colors has-checked:bg-primary has-checked:text-primary-foreground not-has-checked:hover:bg-muted has-disabled:cursor-not-allowed has-disabled:text-subtle-foreground has-focus-visible:outline-2 has-focus-visible:-outline-offset-4 has-focus-visible:outline-current",
            size === "small" && "text-caption",
            i > 0 && "border-l border-input",
          )}
        >
          <input
            type="radio"
            className="sr-only"
            name={name}
            value={option.value}
            checked={option.value === value}
            disabled={option.disabled}
            onChange={() => onChange(option.value)}
          />
          {option.label}
        </label>
      ))}
    </div>
  );
}
