import { useState, type FormEvent } from "react";
import { Link, Navigate } from "react-router-dom";
import {
  ApiError,
  EMAIL_TAKEN_PROBLEM_TYPE,
  errorMessage,
  REGISTRATION_DISABLED_PROBLEM_TYPE,
} from "../api/client";
import { useAuth } from "../auth/AuthProvider";
import { formatDuration } from "../lib/format";
import { useRegistrationEnabled } from "../lib/publicConfig";
import {
  EMAIL_MAX,
  normalizeEmail,
  PASSWORD_MAX,
  PASSWORD_MIN,
  validateRegisterForm,
  type RegisterField,
  type RegisterFormErrors,
  type RegisterFormValues,
} from "../lib/registerForm";

const DISABLED_MESSAGE = "Registration is turned off. Ask the administrator for an account.";

/** Maps a failed registration to field errors and a general message. */
function registerErrors(err: unknown): { fields: RegisterFormErrors; general?: string } {
  if (err instanceof ApiError) {
    if (err.type === EMAIL_TAKEN_PROBLEM_TYPE) {
      return { fields: { email: "This email is already registered. Sign in instead." } };
    }
    if (err.type === REGISTRATION_DISABLED_PROBLEM_TYPE) {
      return { fields: {}, general: DISABLED_MESSAGE };
    }
    if (err.status === 429) {
      return {
        fields: {},
        general: err.retryAfter
          ? `Too many attempts. Try again in ${formatDuration(err.retryAfter)}.`
          : "Too many attempts. Try again later.",
      };
    }
    if (err.status === 422 || err.status === 400) {
      const map = err.fieldErrorMap();
      const fields: RegisterFormErrors = {};
      if (map.email) fields.email = `Email ${map.email}`;
      if (map.password) fields.password = `Password ${map.password}`;
      if (fields.email || fields.password) return { fields };
    }
  }
  return { fields: {}, general: errorMessage(err) };
}

export default function Register() {
  const auth = useAuth();
  const registrationEnabled = useRegistrationEnabled();
  const [values, setValues] = useState<RegisterFormValues>({
    email: "",
    password: "",
    confirm: "",
  });
  const [errors, setErrors] = useState<RegisterFormErrors>({});
  const [general, setGeneral] = useState<string | undefined>();
  const [refused, setRefused] = useState(false);
  const disabled = refused || registrationEnabled === false;
  const [submitting, setSubmitting] = useState(false);

  // After registration the session is set: land on the monitors page (empty state).
  if (auth.session) return <Navigate to="/" replace />;

  function set(field: RegisterField, value: string) {
    setValues((v) => ({ ...v, [field]: value }));
    setErrors((e) => (e[field] ? { ...e, [field]: undefined } : e));
  }

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const found = validateRegisterForm(values);
    setErrors(found);
    setGeneral(undefined);
    if (Object.keys(found).length > 0) return;
    setSubmitting(true);
    try {
      await auth.register(normalizeEmail(values.email), values.password);
    } catch (err) {
      const mapped = registerErrors(err);
      setErrors(mapped.fields);
      setGeneral(mapped.general);
      setRefused(err instanceof ApiError && err.type === REGISTRATION_DISABLED_PROBLEM_TYPE);
      setSubmitting(false);
    }
  }

  const invalid = (field: RegisterField) => (errors[field] ? true : undefined);
  const describedBy = (field: RegisterField, hint?: string) =>
    [errors[field] ? `${field}-error` : null, hint].filter(Boolean).join(" ") || undefined;
  const fieldError = (field: RegisterField) =>
    errors[field] ? <small id={`${field}-error`}>{errors[field]}</small> : null;
  const hasErrors = Object.values(errors).some(Boolean);

  return (
    <article className="login">
      <h1>Create account</h1>
      {disabled ? (
        <>
          <p role="alert" className="error">
            {DISABLED_MESSAGE}
          </p>
          <p>
            <Link to="/login">Go to sign in</Link>
          </p>
        </>
      ) : (
        <form onSubmit={onSubmit} noValidate>
          {(general || hasErrors) && (
            <p role="alert" className="error">
              {general ?? "Check the highlighted fields."}
            </p>
          )}
          <label htmlFor="email">Email</label>
          <input
            id="email"
            name="email"
            type="email"
            autoComplete="email"
            autoCapitalize="none"
            spellCheck={false}
            maxLength={EMAIL_MAX + 10}
            required
            value={values.email}
            onChange={(e) => set("email", e.target.value)}
            aria-invalid={invalid("email")}
            aria-describedby={describedBy("email")}
          />
          {fieldError("email")}

          <label htmlFor="password">Password</label>
          <input
            id="password"
            name="password"
            type="password"
            autoComplete="new-password"
            required
            value={values.password}
            onChange={(e) => set("password", e.target.value)}
            aria-invalid={invalid("password")}
            aria-describedby={describedBy("password", "password-hint")}
          />
          {fieldError("password")}
          <small id="password-hint">
            {PASSWORD_MIN} to {PASSWORD_MAX} characters, not your email. A passphrase works well.
          </small>

          <label htmlFor="confirm">Confirm password</label>
          <input
            id="confirm"
            name="confirm"
            type="password"
            autoComplete="new-password"
            required
            value={values.confirm}
            onChange={(e) => set("confirm", e.target.value)}
            aria-invalid={invalid("confirm")}
            aria-describedby={describedBy("confirm")}
          />
          {fieldError("confirm")}

          <button type="submit" aria-busy={submitting} disabled={submitting}>
            Create account
          </button>
        </form>
      )}
      {registrationEnabled === true && !disabled && (
        <p>
          Already have an account? <Link to="/login">Sign in</Link>
        </p>
      )}
    </article>
  );
}
