import { useState } from "react";
import type { AuthProviders, OAuthProvider } from "../api/endpoints";
import { goToProvider, startErrorMessage } from "../lib/oauth";
import { Alert, Button } from "./ui";

/**
 * One button per configured provider (ADR-0104). A click asks the API for the provider's URL and
 * navigates the whole window there; an invitation token, when given, travels in that request body
 * only.
 */
export default function ProviderButtons({
  providers,
  verb,
  inviteToken,
}: {
  providers: AuthProviders["providers"];
  /** "Sign in with", "Join with". */
  verb: string;
  inviteToken?: string;
}) {
  const [busy, setBusy] = useState<OAuthProvider | null>(null);
  const [error, setError] = useState<string | undefined>();
  if (providers.length === 0) return null;

  async function start(provider: OAuthProvider) {
    setBusy(provider);
    setError(undefined);
    try {
      await goToProvider(provider, inviteToken ? { inviteToken } : {});
    } catch (err) {
      setError(startErrorMessage(err));
      setBusy(null);
    }
  }

  return (
    <div className="flex flex-col gap-2.5">
      {error && <Alert>{error}</Alert>}
      {providers.map((p) => (
        <Button
          key={p.id}
          variant="secondary"
          busy={busy === p.id}
          disabled={busy !== null && busy !== p.id}
          onClick={() => void start(p.id)}
        >
          {verb} {p.name}
        </Button>
      ))}
    </div>
  );
}

/** The divider between the provider buttons and a password form. */
export function OrDivider() {
  return (
    <div className="flex items-center gap-3 text-caption text-muted-foreground" aria-hidden="true">
      <span className="h-px flex-1 bg-border" />
      or
      <span className="h-px flex-1 bg-border" />
    </div>
  );
}
