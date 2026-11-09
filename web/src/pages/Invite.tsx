import { useQuery } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import {
  ALREADY_MEMBER_PROBLEM_TYPE,
  ApiError,
  errorMessage,
  INVITATION_INVALID_PROBLEM_TYPE,
} from "../api/client";
import { lookupInvitation, queryKeys, type InvitationPreview } from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import AuthScreen, { AuthHeading, ShowPassword } from "../components/AuthScreen";
import ProviderButtons, { OrDivider } from "../components/ProviderButtons";
import { useToast } from "../components/Toasts";
import { Alert, Button, buttonClass, cx, Field, Input, Loading, Skeleton } from "../components/ui";
import { formatDateTime, formatDuration } from "../lib/format";
import {
  EMAIL_MAX,
  normalizeEmail,
  validateRegisterForm,
  type RegisterField,
  type RegisterFormErrors,
  type RegisterFormValues,
} from "../lib/registerForm";
import { useAuthProviders, useAuthProvidersQuery } from "../lib/oauth";
import { ROLE_DESCRIPTIONS, ROLE_LABELS } from "../lib/roles";
import { DisplayNameField, PasswordField, registerErrors } from "./Register";

/**
 * The page an invitation link opens (ADR-0102). The token is in the URL fragment, which the
 * browser never sends to a server; it goes to the API only in request bodies (R-213). It is read
 * once into component state and removed from the address bar and the history entry (T-12), and
 * never put into a redirect or router state.
 */
export default function Invite() {
  const location = useLocation();
  const [token] = useState(() => location.hash.replace(/^#/, ""));
  useEffect(() => {
    if (globalThis.location?.hash) {
      globalThis.history.replaceState(globalThis.history.state, "", "/invite");
    }
  }, []);
  const auth = useAuth();
  // Set while joining: the used link must not be looked up again when the session changes.
  const [joining, setJoining] = useState(false);
  const preview = useQuery({
    queryKey: queryKeys.invitationPreview(token),
    queryFn: () => lookupInvitation(token),
    enabled: token !== "" && !joining,
    retry: false,
    gcTime: 0,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  });

  if (token === "") {
    return <Invalid text="This link is incomplete. Copy the whole link you were sent." />;
  }
  if ((preview.isPending && !joining) || auth.status === "loading") {
    return (
      <AuthScreen>
        <Loading label="Checking the invitation…">
          <Skeleton className="h-8 w-48" />
          <Skeleton className="w-full" />
        </Loading>
      </AuthScreen>
    );
  }
  if (preview.isError) {
    const err = preview.error;
    if (err instanceof ApiError && err.status === 429) {
      return (
        <Invalid
          title="Too many attempts."
          text={
            err.retryAfter ? `Try again in ${formatDuration(err.retryAfter)}.` : "Try again later."
          }
        />
      );
    }
    if (err instanceof ApiError && err.type === INVITATION_INVALID_PROBLEM_TYPE) {
      return (
        <Invalid text="It may have expired, been used or been revoked. Ask the person who invited you for a new link." />
      );
    }
    return <Invalid title="Cannot check the invitation." text={errorMessage(err)} />;
  }
  if (!preview.data) return null;
  return auth.session ? (
    <AcceptAsUser token={token} preview={preview.data} onJoining={setJoining} />
  ) : (
    <RegisterInvited token={token} preview={preview.data} />
  );
}

function Invalid({
  title = "This invitation link is not valid.",
  text,
}: {
  title?: string;
  text: string;
}) {
  return (
    <AuthScreen>
      <AuthHeading>Join an organization</AuthHeading>
      <Alert tone="neutral" title={title}>
        {text}
      </Alert>
      <Link to="/login" className={cx(buttonClass(), "mt-1.5")}>
        Go to sign in
      </Link>
    </AuthScreen>
  );
}

function Offer({ preview }: { preview: InvitationPreview }) {
  return (
    <p className="text-muted-foreground">
      You are invited as{" "}
      <strong className="font-medium text-foreground">
        {ROLE_LABELS[preview.role].toLowerCase()}
      </strong>
      : {ROLE_DESCRIPTIONS[preview.role].toLowerCase()} The link works until{" "}
      {formatDateTime(preview.expiresAt)}.
    </p>
  );
}

function AcceptAsUser({
  token,
  preview,
  onJoining,
}: {
  token: string;
  preview: InvitationPreview;
  onJoining: (joining: boolean) => void;
}) {
  const auth = useAuth();
  const navigate = useNavigate();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>();

  async function onJoin() {
    setBusy(true);
    setError(undefined);
    onJoining(true);
    try {
      await auth.acceptInvitation(token);
      navigate("/", { replace: true });
      toast(`You joined ${preview.organizationName}`);
    } catch (err) {
      setBusy(false);
      onJoining(false);
      if (err instanceof ApiError && err.type === ALREADY_MEMBER_PROBLEM_TYPE) {
        setError(
          "You are already a member of this organization, or your account cannot join others. Switch organizations from the top bar instead.",
        );
      } else {
        setError(errorMessage(err));
      }
    }
  }

  return (
    <AuthScreen>
      <AuthHeading>Join {preview.organizationName}</AuthHeading>
      <Offer preview={preview} />
      <p className="text-muted-foreground">
        You are signed in as{" "}
        <strong className="font-mono text-[14px] font-normal break-all text-foreground">
          {auth.session?.user.login}
        </strong>
        . Joining adds this account; your other organizations stay.
      </p>
      {error && <Alert>{error}</Alert>}
      <Button busy={busy} onClick={() => void onJoin()}>
        {busy ? "Joining…" : `Join ${preview.organizationName}`}
      </Button>
      <Button variant="ghost" onClick={() => void auth.logout()}>
        Use another account
      </Button>
    </AuthScreen>
  );
}

/**
 * The provider buttons of the invitation page: the token stays in this component's memory and is
 * sent only in the start request's body (ADR-0104).
 */
function InviteProviders({ token }: { token: string }) {
  const providers = useAuthProviders()?.providers ?? [];
  if (providers.length === 0) return null;
  return (
    <>
      <ProviderButtons providers={providers} verb="Join with" inviteToken={token} />
      <OrDivider />
    </>
  );
}

/** Joining without a session: a new account, or an existing account with its password (T-12). */
function RegisterInvited({ token, preview }: { token: string; preview: InvitationPreview }) {
  const [existing, setExisting] = useState(false);
  const providersQuery = useAuthProvidersQuery();
  const providers = providersQuery.data;
  if (providersQuery.isPending) {
    return (
      <AuthScreen>
        <AuthHeading>Join {preview.organizationName}</AuthHeading>
        <Offer preview={preview} />
        <Loading label="Loading the sign-in options…">
          <Skeleton className="h-11 w-full" />
        </Loading>
      </AuthScreen>
    );
  }
  // When the options cannot be loaded, the password forms stay (the server decides anyway).
  if (providers?.passwordLoginEnabled === false) {
    // Password accounts are off (ADR-0104): the providers are the only way in.
    return (
      <AuthScreen>
        <AuthHeading>Join {preview.organizationName}</AuthHeading>
        <Offer preview={preview} />
        <ProviderButtons providers={providers.providers} verb="Join with" inviteToken={token} />
      </AuthScreen>
    );
  }
  return existing ? (
    <ExistingAccount token={token} preview={preview} onNewAccount={() => setExisting(false)} />
  ) : (
    <NewAccount token={token} preview={preview} onExisting={() => setExisting(true)} />
  );
}

const WRONG_PASSWORD =
  "This email already has an account and the password does not match it. Enter that account's password, or use another email.";

function NewAccount({
  token,
  preview,
  onExisting,
}: {
  token: string;
  preview: InvitationPreview;
  onExisting: () => void;
}) {
  const auth = useAuth();
  const navigate = useNavigate();
  const [values, setValues] = useState<RegisterFormValues>({
    displayName: "",
    email: "",
    password: "",
    confirm: "",
  });
  const [errors, setErrors] = useState<RegisterFormErrors>({});
  const [general, setGeneral] = useState<string | undefined>();
  const [submitting, setSubmitting] = useState(false);
  const [shown, setShown] = useState(false);

  function set(field: RegisterField, value: string) {
    setValues((v) => ({ ...v, [field]: value }));
    setErrors((e) => (e[field] ? { ...e, [field]: undefined } : e));
  }

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const found = validateRegisterForm(values);
    setErrors(found);
    setGeneral(undefined);
    const first = (["displayName", "email", "password", "confirm"] as const).find((f) => found[f]);
    if (first) {
      form.querySelector<HTMLInputElement>(`input[name=${first}]`)?.focus();
      return;
    }
    setSubmitting(true);
    try {
      await auth.register(normalizeEmail(values.email), values.password, token, values.displayName);
      navigate("/", { replace: true });
    } catch (err) {
      if (err instanceof ApiError && err.type === INVITATION_INVALID_PROBLEM_TYPE) {
        setGeneral("The invitation was used, revoked or expired meanwhile. Ask for a new link.");
      } else if (err instanceof ApiError && err.status === 401) {
        setGeneral(WRONG_PASSWORD);
      } else {
        const mapped = registerErrors(err);
        setErrors(mapped.fields);
        setGeneral(mapped.general);
      }
      setSubmitting(false);
    }
  }

  const hasErrors = Object.values(errors).some(Boolean);
  return (
    <AuthScreen
      footer={
        <>
          Already have an account?{" "}
          <Button variant="link" onClick={onExisting}>
            Join with it
          </Button>
        </>
      }
    >
      <AuthHeading>Join {preview.organizationName}</AuthHeading>
      <Offer preview={preview} />
      <InviteProviders token={token} />
      <form onSubmit={onSubmit} noValidate className="flex flex-col gap-[18px]">
        {(general || hasErrors) && <Alert>{general ?? "Check the highlighted fields."}</Alert>}
        <DisplayNameField
          value={values.displayName}
          error={errors.displayName}
          readOnly={submitting}
          onChange={(v) => set("displayName", v)}
        />
        <Field label="Email" error={errors.email}>
          {(c) => (
            <Input
              {...c}
              name="email"
              type="email"
              inputMode="email"
              autoComplete="email"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              maxLength={EMAIL_MAX + 10}
              required
              readOnly={submitting}
              value={values.email}
              onChange={(e) => set("email", e.target.value)}
            />
          )}
        </Field>
        <PasswordField
          values={values}
          error={errors.password}
          shown={shown}
          readOnly={submitting}
          onChange={(v) => set("password", v)}
          onToggle={() => setShown((s) => !s)}
        />
        <Field label="Confirm password" error={errors.confirm}>
          {(c) => (
            <Input
              {...c}
              name="confirm"
              type={shown ? "text" : "password"}
              autoComplete="new-password"
              required
              readOnly={submitting}
              value={values.confirm}
              onChange={(e) => set("confirm", e.target.value)}
            />
          )}
        </Field>
        <Button type="submit" busy={submitting} className="mt-1.5">
          {submitting ? "Creating account…" : "Create account and join"}
        </Button>
      </form>
    </AuthScreen>
  );
}

function ExistingAccount({
  token,
  preview,
  onNewAccount,
}: {
  token: string;
  preview: InvitationPreview;
  onNewAccount: () => void;
}) {
  const auth = useAuth();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [shown, setShown] = useState(false);
  const [error, setError] = useState<string | undefined>();
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(undefined);
    setSubmitting(true);
    try {
      // The registration call with the token checks an existing account's password like a login.
      await auth.register(normalizeEmail(email), password, token);
      navigate("/", { replace: true });
    } catch (err) {
      setSubmitting(false);
      setPassword("");
      if (err instanceof ApiError && (err.status === 401 || err.status === 422)) {
        setError("Wrong email or password. Try again.");
      } else if (err instanceof ApiError && err.type === INVITATION_INVALID_PROBLEM_TYPE) {
        setError("The invitation was used, revoked or expired meanwhile. Ask for a new link.");
      } else if (err instanceof ApiError && err.type === ALREADY_MEMBER_PROBLEM_TYPE) {
        setError(
          "This account is already a member, or cannot join other organizations. Sign in instead.",
        );
      } else if (err instanceof ApiError && err.status === 429) {
        setError(
          err.retryAfter
            ? `Too many attempts. Try again in ${formatDuration(err.retryAfter)}.`
            : "Too many attempts. Try again later.",
        );
      } else {
        setError(errorMessage(err));
      }
    }
  }

  return (
    <AuthScreen
      footer={
        <>
          No account yet?{" "}
          <Button variant="link" onClick={onNewAccount}>
            Create one
          </Button>
        </>
      }
    >
      <AuthHeading>Join {preview.organizationName}</AuthHeading>
      <Offer preview={preview} />
      <p className="text-muted-foreground">
        Sign in with your account to join; your other organizations stay.
      </p>
      <InviteProviders token={token} />
      <form onSubmit={onSubmit} className="flex flex-col gap-[18px]">
        {error && <Alert>{error}</Alert>}
        <Field label="Email">
          {(c) => (
            <Input
              {...c}
              name="email"
              type="email"
              inputMode="email"
              autoComplete="username"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              maxLength={EMAIL_MAX + 10}
              required
              readOnly={submitting}
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          )}
        </Field>
        <Field label="Password">
          {(c) => (
            <Input
              {...c}
              name="password"
              type={shown ? "text" : "password"}
              autoComplete="current-password"
              required
              readOnly={submitting}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              action={
                <ShowPassword shown={shown} controls={c.id} onToggle={() => setShown((v) => !v)} />
              }
            />
          )}
        </Field>
        <Button type="submit" busy={submitting} className="mt-1.5">
          {submitting ? "Joining…" : `Sign in and join ${preview.organizationName}`}
        </Button>
      </form>
    </AuthScreen>
  );
}
