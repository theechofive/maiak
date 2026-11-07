import { useState, type FormEvent } from "react";
import { Link, Navigate, useLocation, useSearchParams } from "react-router-dom";
import { ACCOUNT_SUSPENDED_PROBLEM_TYPE, ApiError, errorMessage } from "../api/client";
import { useAuth } from "../auth/AuthProvider";
import { redirectTarget } from "../auth/RequireAuth";
import AuthScreen, { AuthHeading, authLink, ShowPassword } from "../components/AuthScreen";
import ProviderButtons, { OrDivider } from "../components/ProviderButtons";
import { Alert, Button, Field, Input, Loading, Skeleton } from "../components/ui";
import { formatDuration } from "../lib/format";
import {
  oauthErrorMessage,
  PASSWORD_LOGIN_DISABLED_PROBLEM_TYPE,
  useAuthProvidersQuery,
} from "../lib/oauth";
import { useRegistrationEnabled } from "../lib/publicConfig";

function loginError(err: unknown): { title?: string; text: string } {
  if (err instanceof ApiError && (err.status === 401 || err.status === 400)) {
    return { text: "Wrong login or password. Try again." };
  }
  if (err instanceof ApiError && err.type === ACCOUNT_SUSPENDED_PROBLEM_TYPE) {
    return {
      title: "This account is suspended.",
      text: "Contact the administrator of this Maiak installation.",
    };
  }
  if (err instanceof ApiError && err.type === PASSWORD_LOGIN_DISABLED_PROBLEM_TYPE) {
    return { text: "Password sign-in is turned off. Use one of the sign-in buttons." };
  }
  if (err instanceof ApiError && err.status === 429) {
    return {
      text: err.retryAfter
        ? `Too many attempts. Try again in ${formatDuration(err.retryAfter)}.`
        : "Too many attempts. Try again later.",
    };
  }
  return { text: errorMessage(err) };
}

export default function Login() {
  const auth = useAuth();
  const location = useLocation();
  const registrationEnabled = useRegistrationEnabled();
  const providersQuery = useAuthProvidersQuery();
  const providers = providersQuery.data;
  const [params] = useSearchParams();
  // The callback of an OAuth sign-in comes back with a code, never the provider's text.
  const callbackError = oauthErrorMessage(params.get("error"));
  // /login?admin keeps the break-glass password form when password sign-in is off (ADR-0104).
  const showPassword = providers?.passwordLoginEnabled !== false || params.has("admin");
  const buttons = providers?.providers ?? [];
  const [login, setLogin] = useState("");
  const [password, setPassword] = useState("");
  const [shown, setShown] = useState(false);
  const [error, setError] = useState<{ title?: string; text: string } | null>(null);
  const [submitting, setSubmitting] = useState(false);

  if (auth.session) return <Navigate to={redirectTarget(location.state)} replace />;

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    setSubmitting(true);
    setError(null);
    try {
      // On success the session is set and the redirect above takes over.
      // An email or the system login, case-insensitive; the server lowercases it.
      await auth.login(login.trim(), password);
    } catch (err) {
      setError(loginError(err));
      setSubmitting(false);
      // Wrong credentials keep the login and send the user back to the password. A network
      // error or a rate limit keeps it, so the next try needs no retyping.
      if (err instanceof ApiError && (err.status === 401 || err.status === 400)) {
        setPassword("");
        setShown(false);
        form.querySelector<HTMLInputElement>("input[name=password]")?.focus();
      }
    }
  }

  return (
    <AuthScreen
      footer={
        registrationEnabled === true && (
          <>
            No account yet?{" "}
            <Link to="/register" className={authLink}>
              Create account
            </Link>
          </>
        )
      }
    >
      <AuthHeading>Sign in</AuthHeading>
      {callbackError && !error && <Alert>{callbackError}</Alert>}
      {providersQuery.isPending ? (
        // Until the sign-in methods are known, neither the buttons nor a form that may be off.
        <Loading label="Loading the sign-in options…">
          <Skeleton className="h-11 w-full" />
        </Loading>
      ) : (
        <>
          <ProviderButtons providers={buttons} verb="Sign in with" />
          {buttons.length > 0 && showPassword && <OrDivider />}
          {showPassword && (
            <form onSubmit={onSubmit} className="flex flex-col gap-[18px]">
              {error && <Alert title={error.title}>{error.text}</Alert>}
              <Field label="Email or login">
                {(c) => (
                  <Input
                    {...c}
                    type="text"
                    name="login"
                    autoComplete="username"
                    autoCapitalize="none"
                    autoCorrect="off"
                    spellCheck={false}
                    required
                    readOnly={submitting}
                    value={login}
                    onChange={(e) => setLogin(e.target.value)}
                  />
                )}
              </Field>
              <Field label="Password">
                {(c) => (
                  <Input
                    {...c}
                    type={shown ? "text" : "password"}
                    name="password"
                    autoComplete="current-password"
                    required
                    readOnly={submitting}
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    action={
                      <ShowPassword
                        shown={shown}
                        controls={c.id}
                        onToggle={() => setShown((s) => !s)}
                      />
                    }
                  />
                )}
              </Field>
              <Button type="submit" busy={submitting} className="mt-1.5">
                {submitting ? "Signing in…" : "Sign in"}
              </Button>
            </form>
          )}
        </>
      )}
    </AuthScreen>
  );
}
