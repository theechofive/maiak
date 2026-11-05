import { useId, useState, type FormEvent } from "react";
import { Link, Navigate } from "react-router-dom";
import {
  ApiError,
  EMAIL_TAKEN_PROBLEM_TYPE,
  errorMessage,
  REGISTRATION_DISABLED_PROBLEM_TYPE,
} from "../api/client";
import { useAuth } from "../auth/AuthProvider";
import AuthScreen, { AuthHeading, authLink, ShowPassword } from "../components/AuthScreen";
import {
  Alert,
  Button,
  buttonClass,
  cx,
  DotShape,
  Field,
  HollowDotShape,
  Input,
  TriangleShape,
} from "../components/ui";
import { formatDuration } from "../lib/format";
import { useRegistrationEnabled } from "../lib/publicConfig";
import {
  codePoints,
  EMAIL_MAX,
  normalizeEmail,
  PASSWORD_MAX,
  PASSWORD_MIN,
  validateRegisterForm,
  type RegisterField,
  type RegisterFormErrors,
  type RegisterFormValues,
} from "../lib/registerForm";

const DISABLED_TITLE = "Registration is turned off.";
const DISABLED_TEXT = "Ask the administrator for an account.";

/** Maps a failed registration to field errors and a general message. */
export function registerErrors(err: unknown): { fields: RegisterFormErrors; general?: string } {
  if (err instanceof ApiError) {
    if (err.type === EMAIL_TAKEN_PROBLEM_TYPE) {
      return { fields: { email: "This email is already registered. Sign in instead." } };
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
  const [submitting, setSubmitting] = useState(false);
  const [shown, setShown] = useState(false);
  const disabled = refused || registrationEnabled === false;

  // After registration the session is set: land on the monitors page (empty state).
  if (auth.session) return <Navigate to="/" replace />;

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
      await auth.register(normalizeEmail(values.email), values.password);
    } catch (err) {
      if (err instanceof ApiError && err.type === REGISTRATION_DISABLED_PROBLEM_TYPE) {
        setRefused(true);
      } else {
        const mapped = registerErrors(err);
        setErrors(mapped.fields);
        setGeneral(mapped.general);
      }
      setSubmitting(false);
    }
  }

  if (disabled) {
    return (
      <AuthScreen>
        <AuthHeading>Create account</AuthHeading>
        <Alert tone="neutral" title={DISABLED_TITLE}>
          {DISABLED_TEXT}
        </Alert>
        <Link to="/login" className={cx(buttonClass(), "mt-1.5")}>
          Go to sign in
        </Link>
      </AuthScreen>
    );
  }

  const hasErrors = Object.values(errors).some(Boolean);
  const confirmMatches =
    values.confirm !== "" && values.confirm === values.password && !errors.confirm;

  return (
    <AuthScreen
      footer={
        registrationEnabled === true && (
          <>
            Already have an account?{" "}
            <Link to="/login" className={authLink}>
              Sign in
            </Link>
          </>
        )
      }
    >
      <AuthHeading>Create account</AuthHeading>
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
        <Field
          label="Confirm password"
          error={errors.confirm}
          hint={confirmMatches ? "Matches" : undefined}
        >
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
          {submitting ? "Creating account…" : "Create account"}
        </Button>
      </form>
    </AuthScreen>
  );
}

/** The password with the server's two rules checked as you type. */
export function PasswordField({
  values,
  error,
  shown,
  readOnly,
  onChange,
  onToggle,
}: {
  values: RegisterFormValues;
  error?: string;
  shown: boolean;
  readOnly: boolean;
  onChange: (value: string) => void;
  onToggle: () => void;
}) {
  const policyId = useId();
  const length = codePoints(values.password);
  const email = normalizeEmail(values.email);
  const sameAsEmail =
    values.password !== "" && email !== "" && values.password.toLowerCase() === email;
  const rules: { label: string; state: "todo" | "ok" | "fail"; count?: string }[] = [
    {
      label: `${PASSWORD_MIN} to ${PASSWORD_MAX} characters`,
      state:
        length >= PASSWORD_MIN && length <= PASSWORD_MAX
          ? "ok"
          : length > PASSWORD_MAX || (error && length < PASSWORD_MIN)
            ? "fail"
            : "todo",
      count:
        length === 0
          ? undefined
          : length < PASSWORD_MIN
            ? `${length} / ${PASSWORD_MIN}`
            : String(length),
    },
    { label: "Not your email", state: sameAsEmail ? "fail" : values.password ? "ok" : "todo" },
  ];

  return (
    <Field label="Password" error={error}>
      {(c) => (
        <>
          <Input
            {...c}
            aria-describedby={[c["aria-describedby"], policyId].filter(Boolean).join(" ")}
            name="password"
            type={shown ? "text" : "password"}
            autoComplete="new-password"
            required
            readOnly={readOnly}
            value={values.password}
            onChange={(e) => onChange(e.target.value)}
            action={<ShowPassword shown={shown} plural controls={c.id} onToggle={onToggle} />}
          />
          <div id={policyId} className="flex flex-col gap-1.5 pt-1">
            <ul className="flex flex-col gap-[5px]">
              {rules.map((r) => (
                <li
                  key={r.label}
                  className={cx(
                    "flex items-center gap-2.5 text-caption",
                    r.state === "ok"
                      ? "text-foreground"
                      : r.state === "fail"
                        ? "text-down"
                        : "text-muted-foreground",
                  )}
                >
                  {r.state === "ok" ? (
                    <DotShape className="text-success" />
                  ) : r.state === "fail" ? (
                    <TriangleShape size="small" />
                  ) : (
                    <HollowDotShape />
                  )}
                  <span className="min-w-0 flex-1">
                    {r.label}
                    <span className="sr-only">
                      : {r.state === "ok" ? "met" : r.state === "fail" ? "not met" : "not met yet"}
                    </span>
                  </span>
                  {r.count && (
                    <span aria-hidden="true" className="font-mono text-mono">
                      {r.count}
                    </span>
                  )}
                </li>
              ))}
            </ul>
            <span className="text-caption text-muted-foreground">A passphrase works well.</span>
          </div>
        </>
      )}
    </Field>
  );
}
