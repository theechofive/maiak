import { useState, type FormEvent } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { ApiError, errorMessage } from "../api/client";
import { useAuth } from "../auth/AuthProvider";
import { redirectTarget } from "../auth/RequireAuth";

function loginErrorMessage(err: unknown): string {
  if (err instanceof ApiError && (err.status === 401 || err.status === 400)) {
    return "Wrong email or password. Try again.";
  }
  if (err instanceof ApiError && err.status === 429) {
    return "Too many attempts. Wait a minute and try again.";
  }
  return errorMessage(err);
}

export default function Login() {
  const auth = useAuth();
  const location = useLocation();
  const [email, setEmail] = useState("");
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
      await auth.login(email.trim(), password);
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
          Email
          <input
            type="email"
            name="email"
            autoComplete="username"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
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
    </article>
  );
}
