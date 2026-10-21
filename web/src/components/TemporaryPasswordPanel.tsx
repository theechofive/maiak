import type { ReactNode } from "react";
import { useToast } from "./Toasts";

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
  const toast = useToast();

  async function copy() {
    try {
      await navigator.clipboard.writeText(password);
      toast("Password copied");
    } catch {
      toast("Copying failed. Select the password and copy it by hand.", "error");
    }
  }

  return (
    <article aria-labelledby="temporary-password-heading">
      <h2 id="temporary-password-heading">Temporary password for {login}</h2>
      <p id="temporary-password-warning">
        <strong>Copy it now: it is shown only once.</strong> Maiak keeps only a hash of it. Give it
        to the user over a channel you trust, never in a ticket or a shared chat. All of the
        user&apos;s sessions were ended; they sign in with their email and this password.
      </p>
      <label htmlFor="temporary-password">Temporary password</label>
      <input
        id="temporary-password"
        readOnly
        value={password}
        autoComplete="off"
        spellCheck={false}
        aria-describedby="temporary-password-warning"
        onFocus={(e) => e.currentTarget.select()}
      />
      <div className="actions">
        <button type="button" onClick={() => void copy()}>
          Copy password
        </button>
        {children}
      </div>
    </article>
  );
}
