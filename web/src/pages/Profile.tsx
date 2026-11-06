import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { ApiError, errorMessage } from "../api/client";
import {
  listIdentities,
  queryKeys,
  unlinkIdentity,
  type Identity,
  type OAuthProvider,
} from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import { useToast } from "../components/Toasts";
import {
  Alert,
  Button,
  ConfirmDialog,
  EmptyNote,
  ErrorState,
  Loading,
  Skeleton,
  Table,
  Td,
  Th,
  Tr,
} from "../components/ui";
import { formatDateTime } from "../lib/format";
import {
  goToProvider,
  LAST_LOGIN_METHOD_PROBLEM_TYPE,
  oauthErrorMessage,
  startErrorMessage,
  useAuthProviders,
} from "../lib/oauth";

const LAST_METHOD =
  "This is your only way to sign in. Link another provider first, then remove this one.";

const PROVIDER_NAMES: Record<OAuthProvider, string> = { google: "Google", github: "GitHub" };

/**
 * The user's own sign-in methods (ADR-0104): linked providers, link and unlink. The callback of a
 * link comes back here with ?linked=<provider> or ?error=<code>.
 */
export default function Profile() {
  const auth = useAuth();
  const [params] = useSearchParams();
  const linked = params.get("linked");
  const callbackError = oauthErrorMessage(params.get("error"));
  const isOwner = auth.session?.role === "owner";
  return (
    <section className="flex max-w-[960px] flex-col gap-6">
      <h1 className="pt-6 text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:pt-10 sm:text-display">
        Profile
      </h1>
      <p>
        Signed in as{" "}
        <strong className="font-mono text-[14px] font-normal break-all">
          {auth.session?.user.login}
        </strong>
        .
      </p>
      {linked && !callbackError && (
        <Alert
          tone="neutral"
          title={`${PROVIDER_NAMES[linked as OAuthProvider] ?? linked} linked.`}
        >
          You can now sign in with it.
          {isOwner && linked === "google" && (
            <>
              {" "}
              Claiming a sign-in domain? Continue in{" "}
              <Link to="/settings#sign-in-domains-heading" className="underline underline-offset-3">
                Settings
              </Link>{" "}
              within 10 minutes.
            </>
          )}
        </Alert>
      )}
      {callbackError && <Alert>{callbackError}</Alert>}
      <section aria-labelledby="methods-heading" className="flex flex-col gap-3">
        <h2 id="methods-heading" className="text-lead font-semibold">
          Sign-in methods
        </h2>
        <SignInMethods />
      </section>
    </section>
  );
}

function SignInMethods() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const providers = useAuthProviders();
  const identities = useQuery({ queryKey: queryKeys.identities, queryFn: listIdentities });
  const [confirm, setConfirm] = useState<Identity | null>(null);
  const [error, setError] = useState<string | undefined>();
  const [starting, setStarting] = useState<OAuthProvider | null>(null);

  const unlink = useMutation({
    mutationFn: (provider: OAuthProvider) => unlinkIdentity(provider),
    onSuccess: (_, provider) => {
      setConfirm(null);
      toast(`${PROVIDER_NAMES[provider]} unlinked`);
      void queryClient.invalidateQueries({ queryKey: queryKeys.identities });
    },
    onError: (err) => {
      setConfirm(null);
      setError(
        err instanceof ApiError && err.type === LAST_LOGIN_METHOD_PROBLEM_TYPE
          ? LAST_METHOD
          : errorMessage(err),
      );
    },
  });

  async function link(provider: OAuthProvider) {
    setStarting(provider);
    setError(undefined);
    try {
      await goToProvider(provider, { intent: "link" });
    } catch (err) {
      setError(startErrorMessage(err));
      setStarting(null);
    }
  }

  if (identities.isPending) {
    return (
      <Loading label="Loading your sign-in methods…">
        <Skeleton className="w-full" />
      </Loading>
    );
  }
  if (identities.isError) {
    return (
      <ErrorState
        variant="section"
        title="Cannot load your sign-in methods."
        onRetry={() => void identities.refetch()}
      />
    );
  }
  const data = identities.data;
  const others = (providers?.providers ?? []).filter(
    (p) => !data.items.some((i) => i.provider === p.id),
  );
  const passwordUsable = data.passwordSet && data.passwordLoginEnabled;
  // The server refuses removing the last way in (409); say so before the click.
  const lastMethod = data.items.length === 1 && !passwordUsable;

  return (
    <div className="flex flex-col gap-4">
      {error && <Alert>{error}</Alert>}
      <p className="text-muted-foreground">
        {data.passwordSet
          ? data.passwordLoginEnabled
            ? "You can also sign in with your password."
            : "You have a password, but password sign-in is turned off on this Maiak."
          : "You have no password; you sign in with a provider."}
      </p>
      {data.items.length === 0 ? (
        <EmptyNote>No provider is linked to your account.</EmptyNote>
      ) : (
        <Table>
          <thead>
            <Tr>
              <Th>Provider</Th>
              <Th>Email</Th>
              <Th>Last used</Th>
              <Th>
                <span className="sr-only">Actions</span>
              </Th>
            </Tr>
          </thead>
          <tbody>
            {data.items.map((i) => (
              <Tr key={i.provider}>
                <Td>{PROVIDER_NAMES[i.provider]}</Td>
                <Td className="break-all">{i.email}</Td>
                <Td>{i.lastLoginAt ? formatDateTime(i.lastLoginAt) : "Never"}</Td>
                <Td className="text-right">
                  <Button
                    variant="secondary"
                    size="small"
                    disabled={lastMethod}
                    aria-describedby={lastMethod ? "last-method-hint" : undefined}
                    onClick={() => {
                      setError(undefined);
                      setConfirm(i);
                    }}
                  >
                    Unlink {PROVIDER_NAMES[i.provider]}
                  </Button>
                </Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      )}
      {lastMethod && (
        <p id="last-method-hint" className="text-caption text-muted-foreground">
          {LAST_METHOD}
        </p>
      )}
      {others.length > 0 && (
        <div className="flex flex-wrap gap-2.5">
          {others.map((p) => (
            <Button
              key={p.id}
              variant="secondary"
              busy={starting === p.id}
              disabled={starting !== null && starting !== p.id}
              onClick={() => void link(p.id)}
            >
              Link {p.name}
            </Button>
          ))}
        </div>
      )}
      {confirm && (
        <ConfirmDialog
          title={`Unlink ${PROVIDER_NAMES[confirm.provider]}?`}
          confirmLabel="Unlink"
          busy={unlink.isPending}
          onConfirm={() => unlink.mutate(confirm.provider)}
          onCancel={() => setConfirm(null)}
        >
          You will no longer be able to sign in with this {PROVIDER_NAMES[confirm.provider]}{" "}
          account. You can link it again later.
        </ConfirmDialog>
      )}
    </div>
  );
}
