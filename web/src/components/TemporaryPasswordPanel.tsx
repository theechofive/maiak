import type { ReactNode } from "react";
import { CopyField } from "./ui";

/**
 * The temporary password after a reset by the platform admin, with a copy button. The server keeps
 * only its hash, so this is the only time it can be shown; the caller keeps it in component state
 * only (never in storage), so it is gone when the page is left.
 */
export default function TemporaryPasswordPanel({
  login,
  password,
  children,
}: {
  login: string;
  password: string;
  children?: ReactNode;
}) {
  return (
    <section
      aria-labelledby="temporary-password-heading"
      className="flex flex-col gap-4 border border-border p-5"
    >
      <h3 id="temporary-password-heading" className="text-lead font-semibold">
        Temporary password for {login}
      </h3>
      <CopyField label="Temporary password" value={password} copyLabel="Copy password" />
      <p className="text-muted-foreground">
        Maiak keeps only a hash of it. Give it to the user over a channel you trust, never in a
        ticket or a shared chat. All of the user&apos;s sessions were ended; they sign in with their
        email and this password.
      </p>
      {children && <div className="flex flex-wrap gap-2">{children}</div>}
    </section>
  );
}
