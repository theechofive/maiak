# web

React SPA for Maiak (ADR-0015): Vite, React, TypeScript (strict), React Router, TanStack Query, Pico.css. The API client is generated from `../api/openapi.yaml`.

Requires Node 22 (`.nvmrc`).

## Commands

```bash
npm ci               # install from the lockfile
npm run dev          # dev server on :5173, /api proxied to MAIAK_API_TARGET (default http://localhost:8080)
npm run build        # typecheck + production build into dist/
npm run preview      # serve dist/ locally
npm run lint         # eslint (flat config)
npm run typecheck    # tsc --noEmit
npm test             # vitest (jsdom + Testing Library), fetch is mocked, no network
npm run format       # prettier --write
npm run format:check # prettier --check (CI)
npm run gen:api      # regenerate src/api/generated from ../api/openapi.yaml
```

## Pointing the dev server at an API

`npm run dev` proxies `/api` to `MAIAK_API_TARGET`, so the browser sees one origin and the session cookie works as in production.

```bash
# a backend running locally (cmd/api on :8080)
npm run dev

# a deployed API, e.g. through a port-forward or an internal hostname you can reach
MAIAK_API_TARGET=https://maiak.klr-ops.net npm run dev
```

The session cookie is `Secure`. Against a remote HTTPS target the proxy rewrites the `Host` (`changeOrigin`), but the browser talks to `http://localhost:5173`; browsers accept `Secure` cookies on `localhost`, so this works in Chrome and Firefox. If login succeeds but the next request is 401, the cookie was dropped: use a local API instead.

## API client

`npm run gen:api` runs `openapi-typescript` on `../api/openapi.yaml` and writes `src/api/generated/schema.ts` (formatted with Prettier). The generated file is committed; CI regenerates it and fails when it differs, so run the command after every spec change and commit the result. The npm `overrides` entry lets `openapi-typescript` (peer `typescript@^5`) use the project's TypeScript 6 (L-050).

`src/api/client.ts` wraps it with `openapi-fetch`:

- `apiFetch` adds `X-CSRF-Token` to POST, PUT, PATCH and DELETE and keeps cookies same-origin. The token comes from login or `GET /auth/me` and is kept in memory only (never in `localStorage` or `sessionStorage`); a reload fetches it again.
- Any 401 (except a failed login) calls the handler registered by `AuthProvider`, which drops the session and cached data; the route guard then redirects to `/login` and returns to the original path after sign-in.
- `unwrap` resolves to the response data or throws `ApiError` built from the problem+json body (`status`, `title`, `detail`, `requestId`, `fieldErrors`). An unreachable server is status 0. `errorMessage` turns any error into short UI copy; 501 (operations the backend has not implemented yet) reads "This feature is not available yet."
- `src/api/endpoints.ts` has one function per operation and the TanStack Query keys.

## Layout

- `src/main.tsx` providers (error boundary, Query, Router, auth, toasts); `src/App.tsx` routes
- `src/auth/` session context (`GET /auth/me`), login and logout, `RequireAuth` route guard
- `src/components/` app shell with the top bar, toasts (polite live region), error boundary, loading and error states, the SVG response-time chart
- `src/pages/` one file per page: login, monitors list, add/edit monitor, monitor details, settings
- `src/lib/` pure helpers with unit tests: `format.ts` (all durations, times and numbers; local time zone), `monitors.ts` (status text, summary, filter), `monitorForm.ts` (validation mirroring the server, server field-error mapping), `chart.ts` (scales and gaps), `slack.ts`
- `src/test/utils.tsx` test helpers: `mockApi` routes `fetch` by `METHOD /path` and answers 501 for anything else, like the backend's unimplemented operations
- `src/styles.css` the few custom CSS rules on top of Pico

Data refreshes every 15 s (`POLL_MS`); TanStack Query pauses polling while the tab is hidden. Every page shows its own error state when a request fails, so endpoints that still answer 501 do not break the rest of the page.

## Production image

`Dockerfile` builds the SPA and serves `dist/` with `nginxinc/nginx-unprivileged` on port 8080 as a non-root user; the root filesystem can be read-only. `nginx.conf` provides:

- SPA fallback (`try_files $uri /index.html`); `/healthz` answers 200 from nginx itself
- `Cache-Control: immutable` (1 year) for hashed files in `/assets/`, `no-cache` for `index.html`
- security headers: `nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, and a CSP limited to same origin; scripts are `'self'` only
- all nginx temp paths in `/tmp`, so only `/tmp` must be writable

CSP note: the app uses no inline `style` attributes (the chart is styled through CSS classes) and no inline scripts. `style-src` still allows `'unsafe-inline'` from F-10; it can most likely be dropped after checking the built app in a browser (L-053). The API is same-origin, so `connect-src 'self'` is enough; pointing the production build at another origin would need both CSP and CORS changes.

```bash
docker build -t maiak-web web
docker run --rm --read-only --tmpfs /tmp -p 8080:8080 maiak-web
```
