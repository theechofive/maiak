# ADR-0103: Public status page

- Status: Accepted
- Date: 2026-11-05

## Context
Client 2 wants a public page that shows the health of its services (P-05, deferred since L-004). The incident model was prepared for it (`incident_monitors`, L-092). The page is the first surface that unauthenticated visitors read from tenant data, so what it shows must be a strict projection and its load must not reach the dashboard's database paths. Roles come from ADR-0102.

## Decision
- **Ownership.** A status page belongs to the organization (tenant), not to a project. One page per organization at first (plan limit `max_status_pages`, 1 for every plan); the schema allows several (L-270). Only owners and admins configure it (ADR-0102); other members see the public link.
- **Content.** The admin picks monitors and heartbeats from any project and turns each into a **component** with a public name (1-60) and an optional group name; order is explicit. Internal monitor names, URLs, error kinds, latency and project names are never published.
- **Status.** Component: `operational` (up), `degraded` (failing checks below the failure threshold, or some locations down once there are several), `outage` (down), `no_data` (unknown or paused, excluded from the overall status). Overall: `operational` when every component is operational, `degraded` when the worst is degraded, `partial_outage` when some but not all components are in outage, `major_outage` when all are.
- **History.** 90 daily bars per component (uptime from the hourly rollups, `no_data` for days without checks). Heartbeats have no rollups (L-201): their bars come from incident durations (L-275). Incidents are shown automatically, only for selected monitors: generated public title ("{component} is unavailable"), affected components, start and resolve times. Past incidents of the last 90 days. Manual updates and maintenance notices come later (`incident_updates`, L-267).
- **Data path.**
  - Tables (module `statuspages`, forced RLS, composite keys): `status_pages` (`tenant_id`, `id`, `slug`, `title`, `logo_url`, `accent_color`, `password_hash` nullable, `published`, timestamps, `deleted_at`), `status_page_components` (`tenant_id`, `id`, `page_id`, `monitor_id`, `public_name`, `group_name`, `position`), and `status_page_snapshots` (PK `page_id`, `slug`, `payload jsonb`, `etag`, `generated_at`), the prepared public view.
  - A `jobs` housekeeping task rebuilds the snapshot of every published page every 30 s and right after a configuration change, through a definer function that writes only the allow-listed fields. The builder is the only code that decides what is public; a golden test asserts the exact JSON keys.
  - Deleting a monitor removes its component; a page with no components shows "No components".
- **Renderer.** A new process `statuspage` (`cmd/statuspage`, same image) serves `/status/<slug>`, `/status/<slug>.json`, `/status/<slug>/feed.rss` and `/status/<slug>/badge`. It has no sessions and reads only `status_page_snapshot(slug)`, a definer function, as a new runtime role `maiak_statuspage` with `EXECUTE` on that function and nothing else (created by `maiakctl db bootstrap`, never by migrations). Server-rendered `html/template` (everything escaped, no user HTML), styles built from the design system tokens, colour scheme from `prefers-color-scheme`, English only (L-272). Caching: in-process cache per slug (15 s), `ETag`, `Cache-Control: public, max-age=30`; per-IP rate limits per replica (L-274). The gateway routes `/status` to it on the internal domain first; custom domains (CNAME and certificates) wait for the public-domain move (L-266).
- **Slug.** Chosen by the admin, globally unique, pattern `^[a-z0-9][a-z0-9-]{2,62}$`, reserved words (`api`, `admin`, `status`, `login`, `register`, `assets`, `static`, `health`, `metrics` and similar) refused. A slug is not a secret; an unpublished page answers 404 like an unknown one.
- **Password.** Optional, argon2id hash, at least 12 characters. A correct password sets an HMAC-signed cookie scoped to `/status/<slug>` (key from the generated secrets, ADR-0091); attempts are rate limited per IP. A protected page serves no JSON, RSS or badge and is sent with `Cache-Control: private, no-store`.
- **Branding.** Title, logo URL (`https` only, at most 500 characters, shown with `<img>` and `referrerpolicy=no-referrer`, never fetched by the server) and accent colour (`^#[0-9a-f]{6}$`, applied through a nonce'd style element). The status colours follow the design system, so the accent cannot fake a red "down" block.
- **Security headers.** `Content-Security-Policy: default-src 'none'; style-src 'self' 'nonce-...'; img-src 'self' https:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'` (the badge allows `frame-ancestors *`), `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`. SSRF does not apply (no outbound calls).
- **Feeds and embedding.** RSS 2.0 of incidents (opened and resolved). The badge is a small HTML page for an `<iframe>` ("All systems operational" or the overall state, linking to the page); `/status/<slug>/embed.js` inserts that iframe. The JSON has the overall status, components with their state and bars, and incidents. No email or webhook subscriptions yet (L-268).
- **Audit:** `status_page.created`, `status_page.updated` (field names only), `status_page.published`, `status_page.unpublished`, `status_page.deleted`.

## Consequences
- One more process and database role; the public surface never touches the api's role or sessions, and a renderer bug can only leak what the snapshot already holds.
- Public state lags up to about 45 s (snapshot plus cache). Acceptable for a status page; the dashboard stays live.
- An admin can publish a misleading name or an overly broad selection; the projection limits the damage to public names and up/down history (R-211).
- Supersedes the "subdomain + CDN" idea of L-004 for the first version; L-004 is closed when P-05 is done.

## Alternatives considered
- Rendering the page from the api with live queries: every visitor would hit the read models and the api's role, and a spike would hurt the dashboard.
- A static site pushed to object storage or a CDN: no infrastructure for it in `klr-platform` yet; the snapshot table keeps that option open.
- A status page per project: possible later with the same schema; organizations asked for one page that spans teams.
