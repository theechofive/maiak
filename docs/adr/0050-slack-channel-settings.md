# ADR-0050: Slack channel settings and the Slack notifier

- Status: Accepted
- Date: 2026-10-20

## Context
Task B-13 implements `GET/PUT /settings/slack`, `POST /settings/slack/test` and the Slack sender that `jobs` (B-14) uses. ADR-0016 fixes the URL rule, the message formats and the failure semantics; ADR-0019 fixes routing (one channel per tenant, `notify_slack`, `monitor_channels` unused); ADR-0018 forbids logging or storing raw errors of the guarded client.

## Decision
- **Owner.** Package `internal/notify` owns `notification_channels` (the incidents migration comment called it `internal/notifications`). It holds the settings service, the message formats and `SlackNotifier`. `monitor_channels` is not written (ADR-0019); `PUT` does not touch monitors, `notify_slack` routes them.
- **Validation.** `^https://hooks\.slack\.com/services/[A-Za-z0-9_/\-]+$`, at most 300 characters, and a parse check (scheme, exact host, no port, user info, query or fragment). Failures are 422 with `fieldErrors[webhookUrl]`; the message never repeats the value. The same check runs again before every send.
- **Storage.** AES-256-GCM with the active key (`platform/crypto`); the associated data is `notification_channels.config_encrypted:<channel id>`, so a ciphertext copied to another row or tenant does not decrypt. `PUT` inserts (`ON CONFLICT ... DO NOTHING` on the one-Slack-per-tenant index) or, when a channel exists, locks it and replaces the config under the same id, so queued deliveries follow the new URL; a replaced channel becomes `active` again. Audit `slack.updated` (metadata: `created`, `keyId`) and `slack.tested` (metadata: `result` kind); never the URL.
- **Contract.** `SlackSettings` gains an optional `status` (`active`/`failed`), present when configured (additive). Test endpoint: 204 on success, 409 `slack-not-configured`, 429 with `Retry-After` when Slack rate-limits, 502 `slack-delivery-failed` otherwise, with a detail built only from the error kind. A 404/410 during the test marks the channel `failed`, as a delivery would.
- **Sender.** `SlackNotifier` takes a `Doer`; production uses `notify.NewSlackClient` = `safehttp.NewClient` with `MaxRedirects: 0`, a 10 s deadline and `MAIAK_BLOCKED_CIDRS`. Results: 2xx ok; 404/410 permanent (`webhook_not_found`); 429 retryable with `Retry-After` (seconds or HTTP date, capped at 1 h); any other status, timeout or network error retryable. Every failure is a `*SendError{Kind, Status, Permanent, RetryAfter}` that never wraps the underlying error (which carries the URL and resolved addresses). Slack's body is drained (4 KB) and discarded.
- **Messages** exactly as ADR-0016, mrkdwn-escaped (`&`, `<`, `>`) in names, URLs and reasons; "1 day" instead of "1 days"; durations as `1d 2h 3m` (seconds only below a minute); start time `YYYY-MM-DD HH:MM:SS UTC`.
- **Config.** `MAIAK_PUBLIC_URL` is required by api and jobs: an https URL without trailing slash, user info, query, fragment or link-breaking characters.

## Consequences
- The real guard is not exercised against Slack in tests (it refuses loopback); B-16 adds the route (L-041).
- The web client has to be regenerated for the new optional `status` field (L-080).
- No endpoint deletes the Slack channel; turning alerts off is per monitor (`notify_slack`) (L-081).

## Alternatives considered
- Upsert with `ON CONFLICT DO UPDATE`: the ciphertext must be bound to the row id, which is unknown before the conflict is resolved.
- Returning Slack's response text on test failure: it is untrusted content from a third party and may echo the request.
