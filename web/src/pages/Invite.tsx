import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import {
  ALREADY_MEMBER_PROBLEM_TYPE,
  ApiError,
  errorMessage,
  INVITATION_INVALID_PROBLEM_TYPE,
} from "../api/client";
import { lookupInvitation, queryKeys, type InvitationPreview } from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import AuthScreen, { AuthHeading, authLink } from "../components/AuthScreen";
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
import { ROLE_DESCRIPTIONS, ROLE_LABELS } from "../lib/roles";
import { PasswordField, registerErrors } from "./Register";

/**
 * The page an invitation link opens (ADR-0102). The token is in the URL fragment, which the
 * browser never sends to a server; it goes to the API only in request bodies (R-213).
 */
export default function Invite() {
  const location = useLocation();
  const token = location.hash.replace(/^#/, "");
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

function RegisterInvited({ token, preview }: { token: string; preview: InvitationPreview }) {
  const auth = useAuth();
  const navigate = useNavigate();
  const [values, setValues] = useState<RegisterFormValues>({
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
    const first = (["email", "password", "confirm"] as const).find((f) => found[f]);
    if (first) {
      form.querySelector<HTMLInputElement>(`input[name=${first}]`)?.focus();
      return;
    }
    setSubmitting(true);
    try {
      await auth.register(normalizeEmail(values.email), values.password, token);
      navigate("/", { replace: true });
    } catch (err) {
      if (err instanceof ApiError && err.type === INVITATION_INVALID_PROBLEM_TYPE) {
        setGeneral("The invitation was used, revoked or expired meanwhile. Ask for a new link.");
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
          <Link to="/login" state={{ from: `/invite#${token}` }} className={authLink}>
            Sign in to join
          </Link>
        </>
      }
    >
      <AuthHeading>Join {preview.organizationName}</AuthHeading>
      <Offer preview={preview} />
      <form onSubmit={onSubmit} noValidate className="flex flex-col gap-[18px]">
        {(general || hasErrors) && <Alert>{general ?? "Check the highlighted fields."}</Alert>}
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
