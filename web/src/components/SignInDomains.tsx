import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { ApiError, errorMessage } from "../api/client";
import {
  createAuthDomain,
  deleteAuthDomain,
  listAuthDomains,
  queryKeys,
  updateAuthDomain,
  type AuthDomain,
  type AuthDomainRole,
  type AuthDomainUpdate,
} from "../api/endpoints";
import {
  DOMAIN_NOT_ALLOWED_PROBLEM_TYPE,
  DOMAIN_NOT_VERIFIED_PROBLEM_TYPE,
  DOMAIN_TAKEN_PROBLEM_TYPE,
  goToProvider,
  startErrorMessage,
  useAuthProvidersQuery,
  useRestoredFromCache,
} from "../lib/oauth";
import { ROLE_LABELS } from "../lib/roles";
import { useToast } from "./Toasts";
import {
  Alert,
  Button,
  Checkbox,
  ConfirmDialog,
  EmptyNote,
  ErrorState,
  Field,
  Input,
  Loading,
  Select,
  Skeleton,
} from "./ui";

const DOMAIN_ROLES: AuthDomainRole[] = ["viewer", "member", "admin"];

/** The message for a refused claim (O-04: allow-list, fresh proof, one organization per domain). */
export function claimError(err: unknown): string {
  if (err instanceof ApiError && err.type === DOMAIN_NOT_ALLOWED_PROBLEM_TYPE) {
    return "This Maiak does not allow claiming that domain. The operator lists the domains that may be claimed (MAIAK_OAUTH_ALLOWED_DOMAINS).";
  }
  if (err instanceof ApiError && err.type === DOMAIN_NOT_VERIFIED_PROBLEM_TYPE) {
    return "No recent Google sign-in with an account of this domain. Do step 1, then claim it within 10 minutes.";
  }
  if (err instanceof ApiError && err.type === DOMAIN_TAKEN_PROBLEM_TYPE) {
    return "Another organization on this Maiak has already claimed this domain.";
  }
  if (err instanceof ApiError && err.status === 422) {
    return err.fieldErrors[0]?.message ?? "Enter a domain name such as example.com.";
  }
  return errorMessage(err);
}

/**
 * Google Workspace domains of the organization (ADR-0104, O-04), for owners. Claiming needs a
 * fresh proof: the owner signs in with Google again (the link flow) and claims within 10 minutes.
 */
export default function SignInDomains() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const providersQuery = useAuthProvidersQuery();
  const domains = useQuery({ queryKey: queryKeys.authDomains, queryFn: listAuthDomains });
  const [domain, setDomain] = useState("");
  const [error, setError] = useState<string | undefined>();
  const [proving, setProving] = useState(false);
  const [toDelete, setToDelete] = useState<AuthDomain | null>(null);
  const googleOn = providersQuery.data?.providers.some((p) => p.id === "google") ?? false;
  useRestoredFromCache(useCallback(() => setProving(false), []));
  const refresh = () => void queryClient.invalidateQueries({ queryKey: queryKeys.authDomains });

  const claim = useMutation({
    mutationFn: (d: string) => createAuthDomain(d),
    onSuccess: (d) => {
      setDomain("");
      toast(`${d.domain} claimed`);
      refresh();
    },
    onError: (err) => setError(claimError(err)),
  });
  const update = useMutation({
    mutationFn: ({ id, body }: { id: string; body: AuthDomainUpdate }) =>
      updateAuthDomain(id, body),
    onSuccess: refresh,
    onError: (err) => setError(errorMessage(err)),
  });
  const remove = useMutation({
    mutationFn: (id: string) => deleteAuthDomain(id),
    onSuccess: () => {
      setToDelete(null);
      refresh();
    },
    onError: (err) => {
      setToDelete(null);
      setError(errorMessage(err));
    },
  });

  async function prove() {
    setProving(true);
    setError(undefined);
    try {
      // The link flow refreshes the Google identity's Workspace domain and time (O-04).
      await goToProvider("google", { intent: "link" });
    } catch (err) {
      setError(startErrorMessage(err));
      setProving(false);
    }
  }

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(undefined);
    const value = domain.trim().toLowerCase();
    if (value === "") {
      setError("Enter a domain name such as example.com.");
      return;
    }
    claim.mutate(value);
  }

  return (
    <div className="flex flex-col gap-5">
      {error && <Alert>{error}</Alert>}
      {domains.isPending ? (
        <Loading label="Loading sign-in domains…">
          <Skeleton className="w-full" />
        </Loading>
      ) : domains.isError ? (
        <ErrorState
          variant="section"
          title="Cannot load the sign-in domains."
          onRetry={() => void domains.refetch()}
        />
      ) : domains.data.items.length === 0 ? (
        <EmptyNote>No domain is claimed. New people join only through invitations.</EmptyNote>
      ) : (
        // A list, not a table: the four columns do not fit a phone (O-05 browser check).
        <ul aria-label="Claimed domains" className="flex flex-col border-t border-border">
          {domains.data.items.map((d) => (
            <li key={d.id} className="flex flex-col gap-3 border-b border-border py-3">
              <span className="font-mono break-all">{d.domain}</span>
              <div className="flex flex-wrap items-center gap-x-5 gap-y-3">
                <span className="flex items-center gap-2">
                  <span aria-hidden="true" className="text-caption text-muted-foreground">
                    Joins as
                  </span>
                  <Select
                    aria-label={`Role for ${d.domain}`}
                    value={d.defaultRole}
                    disabled={update.isPending}
                    onChange={(e) =>
                      update.mutate({
                        id: d.id,
                        body: { defaultRole: e.target.value as AuthDomainRole },
                      })
                    }
                  >
                    {DOMAIN_ROLES.map((r) => (
                      <option key={r} value={r}>
                        {ROLE_LABELS[r]}
                      </option>
                    ))}
                  </Select>
                </span>
                <Checkbox
                  label={
                    <>
                      Auto-join<span className="sr-only"> for {d.domain}</span>
                    </>
                  }
                  checked={d.enabled}
                  disabled={update.isPending}
                  onChange={(e) => update.mutate({ id: d.id, body: { enabled: e.target.checked } })}
                />
                <Button
                  variant="secondary"
                  size="small"
                  className="sm:ml-auto"
                  onClick={() => {
                    setError(undefined);
                    setToDelete(d);
                  }}
                >
                  Release {d.domain}
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}

      <div className="flex flex-col gap-3 border-t border-border-faint pt-5">
        <h3 className="font-semibold">Claim a domain</h3>
        {providersQuery.isPending ? (
          <Loading label="Loading the sign-in options…">
            <Skeleton className="w-full" />
          </Loading>
        ) : providersQuery.isError ? (
          <ErrorState
            variant="section"
            title="Cannot load the sign-in options."
            onRetry={() => void providersQuery.refetch()}
          />
        ) : !googleOn ? (
          <p className="text-muted-foreground">
            Google sign-in is not configured on this Maiak, so domains cannot be claimed.
          </p>
        ) : (
          <ol className="flex list-decimal flex-col gap-4 pl-5">
            <li className="pl-1">
              <div className="flex flex-col gap-2.5">
                <span>
                  Sign in with Google again with an account of the domain; this proves you control
                  it for 10 minutes. If another Google account is already linked to you, unlink it
                  under{" "}
                  <Link to="/profile" className="underline underline-offset-3">
                    Sign-in methods
                  </Link>{" "}
                  first, or use that account. You come back to your profile page; its Settings link
                  brings you back here.
                </span>
                <Button
                  variant="secondary"
                  className="self-start"
                  busy={proving}
                  onClick={() => void prove()}
                >
                  Sign in with Google again
                </Button>
              </div>
            </li>
            <li className="pl-1">
              <div className="flex flex-col gap-2.5">
                <span>
                  Enter the domain and claim it. A new domain lets people join as viewers; raise the
                  role afterwards.
                </span>
                <form onSubmit={onSubmit} className="flex flex-col gap-3 sm:flex-row sm:items-end">
                  <Field label="Domain" className="flex-1">
                    {(c) => (
                      <Input
                        {...c}
                        name="domain"
                        mono
                        autoCapitalize="none"
                        autoCorrect="off"
                        spellCheck={false}
                        placeholder="example.com"
                        maxLength={253}
                        value={domain}
                        readOnly={claim.isPending}
                        onChange={(e) => setDomain(e.target.value)}
                      />
                    )}
                  </Field>
                  <Button type="submit" busy={claim.isPending}>
                    Claim domain
                  </Button>
                </form>
              </div>
            </li>
          </ol>
        )}
      </div>

      {toDelete && (
        <ConfirmDialog
          title={`Release ${toDelete.domain}?`}
          confirmLabel="Release"
          busy={remove.isPending}
          onConfirm={() => remove.mutate(toDelete.id)}
          onCancel={() => setToDelete(null)}
        >
          People of this domain will no longer join on their first sign-in. Members who already
          joined stay. Another organization may claim the domain afterwards.
        </ConfirmDialog>
      )}
    </div>
  );
}
