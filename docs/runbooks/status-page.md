# Runbook: public status page

Decision: [ADR-0103](../adr/0103-status-page.md) (with implementation notes). Organizations configure the page in the app (**Status page**, owners and admins); visitors open `https://<host>/status/<slug>`, served by the `statuspage` process.

## Enabling it on an existing installation (klr-platform)
The renderer logs in as `maiak_statuspage`, which only `maiakctl db bootstrap` creates, and reads its URL from the key `statuspage_url` of the Secret `maiak-db`. Installations from before P-05 have neither, so `statuspage.enabled` is `false` in `values-platform.yaml` (L-292). The admin API, the settings page and the snapshots work without it; only the public page is missing.

1. Deploy the P-05 release as usual (migration `20261106120000_status_pages`). Check: `kubectl --context <klr-platform context> -n maiak logs job/maiak-migrate`.
2. Set `dbBootstrap.enabled: true` in `values-platform.yaml` and sync once. The Job creates `maiak_statuspage`, adds `statuspage_password` and `statuspage_url` to `maiak-db` (existing keys are never rewritten; only key names are logged) and grants `EXECUTE` on `status_page_snapshot(text)`. While enabled the Job holds the RDS master login (R-009).
3. Set `dbBootstrap.enabled: false` again and `statuspage.enabled: true` in the same commit; sync. With `secrets.statuspage.source: generate` (default) the secrets bootstrap Job creates the Secret `maiak-statuspage` (key `cookie_key`) first; with `externalSecret` create `platform/maiak/statuspage` (property `cookie_key`, standard base64 of 32 random bytes) before.
4. Verify: `kubectl --context <klr-platform context> -n maiak get pods -l app.kubernetes.io/component=statuspage` is Ready; `curl -sI https://maiak.klr-ops.net/status/does-not-exist` answers 404 with a `Content-Security-Policy` header; publish a page and open it.

## Checking what the role may do
As a superuser on the database (read-only queries):
```sql
SELECT has_function_privilege('maiak_statuspage', 'status_page_snapshot(text)', 'EXECUTE');      -- true
SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
 WHERE has_table_privilege('maiak_statuspage', c.oid, 'SELECT');                                   -- 0
```

## A page shows old data
- The renderer caches 15 s per replica; jobs rebuilds every 30 s (`maiak_jobs_housekeeping_runs_total{task="rebuild_status_pages"}`, `maiak_jobs_status_page_snapshots_total`). A configuration change is rebuilt by the api at once.
- jobs logs "status page snapshot rebuild failed" with the tenant and page id when one page fails; the others continue.
- A suspended or deleted organization's page answers 404 (by design).

## Timing of changes
The api rebuilds a page's snapshot with every change (and when a monitor on it is paused, resumed or deleted); the renderer caches each page 15 s per replica and answers with `max-age=30`. So publishing, unpublishing, deleting and setting or removing a password apply within about a minute, not at once: a visitor or a cache may still see the previous state until then. To take a page down urgently, unpublish it and expect up to a minute.

## Addresses
A deleted page's address, and the old address after a rename, stay reserved for the organization for 30 days (`status_page_released_slugs`); the organization may reuse them, others get "This address is not available", the same answer as for an address in use. To free one early for someone else (rare, on request of the organization that released it), delete its row as `maiak_owner` with `app.tenant_id` set to that organization.

## A visitor cannot open a protected page
- Wrong passwords and 429s are counted in `maiak_statuspage_unlock_attempts_total` and `maiak_statuspage_rate_limited_total{kind="unlock"}`: 10 attempts per client (IPv4 address or IPv6 /64) and 100 wrong passwords per page per 15 minutes, per replica. When the page budget is used up, each client still has 3 wrong passwords of its own, so a visitor with the right password gets in during an attack.
- Changing the password signs every visitor out; so does rotating the cookie key (delete the Secret `maiak-statuspage`, sync, restart the `statuspage` Deployment).
- Behind the gateway the client IP comes from `X-Forwarded-For` (`config.trustedProxyHops`); without it every visitor shares one limit.
- Limits trust `X-Forwarded-For` as the api does: requests without it all share one `unknown` bucket, and a pod inside the cluster can reach the `statuspage` Service directly (NetworkPolicy is not enforced, R-002) and send a forged `X-Forwarded-For` to get a fresh bucket for every request. The limits therefore only hold for traffic through the gateway; treat them as a cost bound, not a protection against in-cluster clients.

## Abuse
- A page shows only what its admin published: public names, groups, states, daily bars and generated incident titles (R-211). Ask the organization's admin to rename or remove a component, or unpublish the page. As a last resort the platform admin suspends the organization (its page then answers 404).
- Request floods are limited per client IP and replica (L-274); scale replicas only together with a look at L-274.

## Things that are not there yet
Custom domains (L-266), manual incident updates and maintenance (L-267), subscriptions other than RSS (L-268), several pages per organization (L-270), translations and custom styling (L-272).
