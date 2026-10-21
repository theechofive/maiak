import { useState, type FormEvent } from "react";
import { Link, Navigate, useLocation } from "react-router-dom";
import { ApiError, errorMessage } from "../api/client";
import { useAuth } from "../auth/AuthProvider";
import { redirectTarget } from "../auth/RequireAuth";
import { formatDuration } from "../lib/format";
import { useRegistrationEnabled } from "../lib/publicConfig";

function loginErrorMessage(err: unknown): string {
  if (err instanceof ApiError && (err.status === 401 || err.status === 400)) {
    return "Wrong login or password. Try again.";
  }
  if (err instanceof ApiError && err.status === 429) {
    return err.retryAfter
      ? `Too many attempts. Try again in ${formatDuration(err.retryAfter)}.`
      : "Too many attempts. Try again later.";
  }
  return errorMessage(err);
}

export default function Login() {
  const auth = useAuth();
  const location = useLocation();
  const registrationEnabled = useRegistrationEnabled();
  const [login, setLogin] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  if (auth.session) return <Navigate to={redirectTarget(location.state)} replace />;

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      // On success the session is set and the redirect above takes over.
      // An email or the system login, case-insensitive; the server lowercases it.
      await auth.login(login.trim(), password);
    } catch (err) {
      setError(loginErrorMessage(err));
      setSubmitting(false);
    }
  }

  return (
    <article className="login">
      <h1>Sign in</h1>
      <form onSubmit={onSubmit}>
        <label>
          Email or login
          <input
            type="text"
            name="login"
            autoComplete="username"
            autoCapitalize="none"
            spellCheck={false}
            required
            value={login}
            onChange={(e) => setLogin(e.target.value)}
          />
        </label>
        <label>
          Password
          <input
            type="password"
            name="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        {error && (
          <p role="alert" className="error">
            {error}
          </p>
        )}
        <button type="submit" aria-busy={submitting} disabled={submitting}>
          Sign in
        </button>
      </form>
      {registrationEnabled === true && (
        <p>
          No account yet? <Link to="/register">Create account</Link>
        </p>
      )}
    </article>
  );
}
