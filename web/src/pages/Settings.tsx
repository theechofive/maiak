import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { ApiError, errorMessage } from "../api/client";
import {
  getSlackSettings,
  putSlackSettings,
  queryKeys,
  testSlack,
  type Usage,
} from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import { useToast } from "../components/Toasts";
import {
  Alert,
  Button,
  cx,
  DotShape,
  ErrorState,
  Field,
  Input,
  Meter,
  Skeleton,
  TriangleShape,
  WriteOnlyValue,
  Loading,
  Segmented,
} from "../components/ui";
import { formatClock } from "../lib/format";
import { validateSlackUrl } from "../lib/slack";
import { useTheme } from "../lib/theme";
import { useUsage } from "../lib/usage";

export default function Settings() {
  const auth = useAuth();
  return (
    <section className="flex max-w-[960px] flex-col">
      <h1 className="pt-6 pb-6 text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:pt-10 sm:pb-9 sm:text-display">
        Settings
      </h1>
      <SettingsSection id="plan" title="Plan">
        <PlanUsage />
      </SettingsSection>
      <SettingsSection
        id="slack"
        title="Slack"
        description="Maiak posts to one Slack channel when a monitor goes down, when it recovers and when a TLS certificate is about to expire."
      >
        <SlackSettings />
      </SettingsSection>
      <SettingsSection id="appearance" title="Appearance" description="Saved in this browser only.">
        <Appearance />
      </SettingsSection>
      <SettingsSection id="account" title="Account">
        <div className="flex flex-col gap-1">
          <p>
            Signed in as{" "}
            <strong className="font-mono text-[14px] font-normal break-all">
              {auth.session?.user.login}
            </strong>
            . The password is managed outside the app.
          </p>
        </div>
      </SettingsSection>
    </section>
  );
}

function SettingsSection({
  id,
  title,
  description,
  children,
}: {
  id: string;
  title: string;
  description?: string;
  children: ReactNode;
}) {
  return (
    <section
      aria-labelledby={`${id}-heading`}
      className="grid gap-4 border-t border-border pt-6 pb-7 sm:grid-cols-[220px_minmax(0,1fr)] sm:gap-12 sm:pt-8 sm:pb-10"
    >
      <div className="flex flex-col gap-1.5">
        <h2 id={`${id}-heading`} className="text-lead font-semibold">
          {title}
        </h2>
        {description && <p className="text-[13px] text-muted-foreground">{description}</p>}
      </div>
      <div className="flex min-w-0 max-w-[560px] flex-col">{children}</div>
    </section>
  );
}

const usageRows: { key: keyof Usage["limits"]; label: string }[] = [
  { key: "monitors", label: "Monitors" },
  { key: "heartbeats", label: "Heartbeat monitors" },
  { key: "channels", label: "Notification channels" },
];

function PlanUsage() {
  const usage = useUsage();
  if (usage.isPending) {
    return (
      <Loading label="Loading plan and usage…" className="gap-5 pt-1">
        <Skeleton className="h-5 w-28" />
        {usageRows.map((r) => (
          <Skeleton key={r.key} className="w-full" />
        ))}
      </Loading>
    );
  }
  if (usage.isError && !usage.data) {
    return (
      <ErrorState
        variant="section"
        title="Cannot load plan and usage"
        onRetry={() => void usage.refetch()}
      />
    );
  }
  return (
    <>
      <p className="pb-2 text-muted-foreground">
        Your tenant is on the{" "}
        <strong className="text-[22px] font-semibold tracking-[-0.015em] text-foreground">
          {usage.data.plan.name}
        </strong>{" "}
        plan.
      </p>
      <div className="divide-y divide-border-faint border-t border-border-faint">
        {usageRows.map(({ key, label }) => (
          <Meter
            key={key}
            label={label}
            used={usage.data.usage[key]}
            limit={usage.data.limits[key]}
          />
        ))}
      </div>
    </>
  );
}

type TestResult = { ok: true; at: number } | { ok: false; message: string };

function SlackSettings() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const settings = useQuery({ queryKey: queryKeys.slack, queryFn: getSlackSettings });
  const [webhookUrl, setWebhookUrl] = useState("");
  const [error, setError] = useState<string | undefined>();
  const [replacing, setReplacing] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  // Replacing is a deliberate step: the field it opens takes the focus.
  useEffect(() => {
    if (replacing) input.current?.focus();
  }, [replacing]);
  const [result, setResult] = useState<TestResult | undefined>();

  const save = useMutation({
    mutationFn: (url: string) => putSlackSettings(url),
    onSuccess: (data) => {
      queryClient.setQueryData(queryKeys.slack, data);
      // A new channel counts against the plan.
      void queryClient.invalidateQueries({ queryKey: queryKeys.usage });
      // The URL is a secret: it is never shown again after saving.
      setWebhookUrl("");
      // Drop the URL from the mutation variables too.
      save.reset();
      setReplacing(false);
      setResult(undefined);
      toast("Webhook saved");
    },
    onError: (err) => {
      const fieldMessage = err instanceof ApiError ? err.fieldErrorMap().webhookUrl : undefined;
      setError(fieldMessage ?? errorMessage(err));
    },
  });

  const test = useMutation({
    mutationFn: testSlack,
    onSuccess: () => setResult({ ok: true, at: Date.now() }),
    onError: (err) => setResult({ ok: false, message: errorMessage(err) }),
  });

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const found = validateSlackUrl(webhookUrl);
    setError(found);
    if (!found) save.mutate(webhookUrl.trim());
  }

  if (settings.isPending) {
    return (
      <Loading label="Loading Slack settings…" className="gap-4">
        <Skeleton className="w-3/4" />
        <Skeleton className="h-9 w-full" />
      </Loading>
    );
  }

  const configured = settings.data?.configured ?? false;
  const revoked = configured && settings.data?.status === "failed";
  const showForm = !configured || replacing;

  return (
    <div className="flex flex-col gap-6">
      {settings.isError ? (
        <ErrorState
          variant="section"
          title="Cannot load Slack settings"
          onRetry={() => void settings.refetch()}
        />
      ) : (
        <>
          <Steps configured={configured} revoked={revoked} result={result} />
          <p className={cx(revoked && "text-down")}>
            {!configured
              ? "No webhook is configured. Slack notifications are off."
              : revoked
                ? "A webhook is configured, but Slack refused it: it was probably removed in Slack. Alerts are not sent until you save a new URL."
                : "A webhook is configured. Alerts go to its channel."}
          </p>
        </>
      )}

      {showForm ? (
        <form onSubmit={onSubmit} noValidate className="flex flex-col gap-4">
          <Field
            label={replacing ? "New Slack webhook URL" : "Slack webhook URL"}
            hint="Create an Incoming Webhook in Slack and paste its URL. It is stored encrypted and never shown again after saving."
            error={error}
          >
            {(c) => (
              <Input
                {...c}
                name="webhookUrl"
                type="text"
                ref={input}
                mono
                secret
                autoComplete="off"
                spellCheck={false}
                autoCapitalize="off"
                placeholder="https://hooks.slack.com/services/…"
                value={webhookUrl}
                onChange={(e) => {
                  setWebhookUrl(e.target.value);
                  setError(undefined);
                }}
              />
            )}
          </Field>
          <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
            <Button type="submit" busy={save.isPending}>
              {save.isPending ? "Saving…" : replacing ? "Save new URL" : "Save"}
            </Button>
            {replacing && (
              <Button
                variant="ghost"
                onClick={() => {
                  setReplacing(false);
                  setWebhookUrl("");
                  setError(undefined);
                }}
              >
                Cancel
              </Button>
            )}
          </div>
        </form>
      ) : (
        <WriteOnlyValue
          label="Webhook URL"
          hint="Stored encrypted. It cannot be shown again, only replaced."
          onReplace={() => setReplacing(true)}
        />
      )}

      <div className="flex flex-col gap-3">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
          <Button
            variant={configured && !result && !revoked ? "primary" : "secondary"}
            busy={test.isPending}
            disabled={!configured}
            className="self-start"
            onClick={() => test.mutate()}
          >
            {test.isPending ? "Sending…" : result ? "Send another test" : "Send test message"}
          </Button>
          {result?.ok && (
            <span role="status" className="flex items-center gap-2 text-[13px]">
              <DotShape size="large" className="text-success" />
              Test message sent at {formatClock(result.at)}. Look for it in your Slack channel.
            </span>
          )}
        </div>
        {result && !result.ok && (
          <Alert title="Test message failed.">
            {result.message}
            {configured && !replacing && (
              <>
                {" "}
                If the webhook was removed in Slack,{" "}
                <Button variant="link" onClick={() => setReplacing(true)}>
                  replace the URL
                </Button>
                .
              </>
            )}
          </Alert>
        )}
      </div>
    </div>
  );
}

/** Not configured → Saved → Test delivered (or refused), so the next step is always visible. */
function Steps({
  configured,
  revoked,
  result,
}: {
  configured: boolean;
  revoked: boolean;
  result: TestResult | undefined;
}) {
  const failed = revoked || (result !== undefined && !result.ok);
  const delivered = !failed && result?.ok === true;
  const steps = [
    {
      label: "Not configured",
      sub: configured ? "done" : "now",
      done: configured,
      current: !configured,
    },
    {
      label: "Saved",
      sub: "encrypted",
      done: configured && (delivered || failed),
      current: configured && !delivered && !failed,
    },
    failed
      ? {
          label: "Test failed",
          sub: revoked ? "Slack refused the webhook" : "see below",
          failed: true,
        }
      : {
          label: "Test delivered",
          sub: delivered && result?.ok ? formatClock(result.at) : "not sent yet",
          done: delivered,
          current: delivered,
        },
  ];
  return (
    <ol aria-label="Slack connection" className="grid grid-cols-3 gap-0">
      {steps.map((step, i) => (
        <li
          key={step.label}
          aria-current={"current" in step && step.current ? "step" : undefined}
          className="flex min-w-0 flex-col gap-3"
        >
          <div className="flex h-3.5 items-center">
            {"failed" in step && step.failed ? (
              <TriangleShape size="large" className="text-down" />
            ) : (
              <span
                aria-hidden="true"
                className={cx(
                  "box-border size-2.5 flex-none rounded-full",
                  step.current
                    ? "bg-foreground shadow-[0_0_0_3px_var(--background),0_0_0_4px_var(--foreground)]"
                    : step.done
                      ? "bg-foreground"
                      : "border-[1.5px] border-input",
                )}
              />
            )}
            {i < steps.length - 1 && (
              <span
                aria-hidden="true"
                className={cx(
                  "mx-2.5 h-px flex-1",
                  "failed" in steps[i + 1]! && steps[i + 1]!.failed
                    ? "bg-down"
                    : step.done
                      ? "bg-foreground"
                      : "bg-border",
                )}
              />
            )}
          </div>
          <div className="flex flex-col gap-0.5 pr-2.5">
            <span
              className={cx(
                "text-[13px]",
                "failed" in step && step.failed
                  ? "font-semibold text-down"
                  : step.current
                    ? "font-semibold"
                    : step.done
                      ? ""
                      : "text-muted-foreground",
              )}
            >
              {step.label}
            </span>
            <span className="font-mono text-[11px] text-muted-foreground">{step.sub}</span>
          </div>
        </li>
      ))}
    </ol>
  );
}

function Appearance() {
  const [theme, setTheme] = useTheme();
  return (
    <div className="flex flex-col items-start gap-2">
      <Segmented
        label="Theme"
        name="theme"
        value={theme}
        onChange={setTheme}
        options={[
          { value: "system", label: "System" },
          { value: "light", label: "Light" },
          { value: "dark", label: "Dark" },
        ]}
      />
      <span className="text-caption text-muted-foreground">
        System follows your device&apos;s light or dark mode.
      </span>
    </div>
  );
}
