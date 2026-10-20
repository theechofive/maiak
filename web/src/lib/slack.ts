export const SLACK_PREFIX = "https://hooks.slack.com/services/";

/** Mirrors the server rule (ADR-0016): only Slack Incoming Webhook URLs are accepted. */
export function validateSlackUrl(raw: string): string | undefined {
  const value = raw.trim();
  if (!value) return "Enter the webhook URL.";
  if (value.length > 2048) return "Use 2048 characters or fewer.";
  if (!value.startsWith(SLACK_PREFIX) || value.length === SLACK_PREFIX.length) {
    return `Use a Slack Incoming Webhook URL that starts with ${SLACK_PREFIX}`;
  }
  return undefined;
}
