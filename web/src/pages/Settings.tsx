import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiError, errorMessage } from "../api/client";
import { getSlackSettings, putSlackSettings, queryKeys, testSlack } from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import { ErrorState, Loading } from "../components/QueryState";
import { useToast } from "../components/Toasts";
import { validateSlackUrl } from "../lib/slack";

export default function Settings() {
  const auth = useAuth();
  return (
    <section>
      <h1>Settings</h1>
      <SlackSettings />
      <section aria-labelledby="account-heading">
        <h2 id="account-heading">Account</h2>
        <p>
          Signed in as <strong>{auth.session?.user.login}</strong>. The password is managed outside
          the app.
        </p>
      </section>
    </section>
  );
}

function SlackSettings() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const settings = useQuery({ queryKey: queryKeys.slack, queryFn: getSlackSettings });
  const [webhookUrl, setWebhookUrl] = useState("");
  const [error, setError] = useState<string | undefined>();

  const save = useMutation({
    mutationFn: (url: string) => putSlackSettings(url),
    onSuccess: (data) => {
      queryClient.setQueryData(queryKeys.slack, data);
      // The URL is a secret: it is never shown again after saving.
      setWebhookUrl("");
      // Drop the URL from the mutation variables too.
      save.reset();
      toast("Webhook saved");
    },
    onError: (err) => {
      const fieldMessage = err instanceof ApiError ? err.fieldErrorMap().webhookUrl : undefined;
      setError(fieldMessage ?? errorMessage(err));
    },
  });

  const test = useMutation({
    mutationFn: testSlack,
    onSuccess: () => toast("Test message sent"),
    onError: (err) => toast(`Test message failed. ${errorMessage(err)}`, "error"),
  });

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const found = validateSlackUrl(webhookUrl);
    setError(found);
    if (!found) save.mutate(webhookUrl.trim());
  }

  const configured = settings.data?.configured ?? false;

  return (
    <section aria-labelledby="slack-heading">
      <h2 id="slack-heading">Slack</h2>
      <p>
        Maiak posts to one Slack channel when a monitor goes down, when it recovers and when a TLS
        certificate is about to expire. Create an Incoming Webhook in Slack for that channel and
        paste its URL here.
      </p>
      {settings.isPending ? (
        <Loading label="Loading Slack settings…" />
      ) : settings.isError ? (
        <ErrorState
          title="Cannot load Slack settings"
          error={settings.error}
          onRetry={() => void settings.refetch()}
        />
      ) : (
        <p>
          {configured
            ? "A webhook is configured. Enter a new URL to replace it."
            : "No webhook is configured. Slack notifications are off."}
        </p>
      )}
      <form onSubmit={onSubmit} noValidate>
        <label htmlFor="webhookUrl">Slack webhook URL</label>
        <input
          id="webhookUrl"
          name="webhookUrl"
          type="text"
          className="secret-input"
          autoComplete="off"
          spellCheck={false}
          placeholder={configured ? "Configured, hidden" : "https://hooks.slack.com/services/…"}
          value={webhookUrl}
          onChange={(e) => {
            setWebhookUrl(e.target.value);
            setError(undefined);
          }}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? "webhookUrl-error webhookUrl-hint" : "webhookUrl-hint"}
        />
        {error && <small id="webhookUrl-error">{error}</small>}
        <small id="webhookUrl-hint">
          The URL is stored encrypted and is never shown again after saving.
        </small>
        <div className="actions">
          <button type="submit" aria-busy={save.isPending} disabled={save.isPending}>
            Save
          </button>
          <button
            type="button"
            className="secondary"
            aria-busy={test.isPending}
            disabled={!configured || test.isPending}
            onClick={() => test.mutate()}
          >
            Send test message
          </button>
        </div>
      </form>
    </section>
  );
}
