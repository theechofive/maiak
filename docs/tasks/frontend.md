# Frontend tasks for the MVP

Stack and scope: [ADR-0015](../adr/0015-frontend-stack.md). Pages follow the agreed wireframes. The API contract is the table in [README.md](README.md); the generated client comes from `api/openapi.yaml` (task B-05), so frontend work can start from the contract and use mocks until the backend endpoints exist.

| ID | Task | Depends on | Status |
|---|---|---|---|
| F-01 | Scaffold `web/`: Vite + React + TypeScript, React Router, TanStack Query, Pico.css, ESLint, Prettier, Vitest + Testing Library; dev proxy of `/api` to a configurable target | none | done |
| F-02 | API client generation: `npm run gen:api` creates types and `openapi-fetch` client from `../api/openapi.yaml`; CI check that generated files are current; a small fetch wrapper that adds the CSRF header and turns problem+json into typed errors | B-05 | todo |
| F-03 | Auth: login page, session bootstrap through `GET /auth/me`, route guard, logout, redirect to login on any 401, CSRF token kept in memory | F-01, F-02 | todo |
| F-04 | App shell: top bar (Monitors, Settings, user, Log out), layout, global error and loading states, toasts for save results | F-03 | todo |
| F-05 | Monitors list page: summary line (up, down, paused), filter by name or URL, status with duration, uptime 24 h, response time, last checked, row click opens details, empty state, polling every 15 s | F-04 | todo |
| F-06 | Add and edit monitor form: name, URL (https validation), fixed interval and timeout shown as read-only, expected status, failure threshold, notify Slack checkbox; server validation errors shown next to fields | F-04 | todo |
| F-07 | Monitor details page: header with Edit, Pause or Resume, Delete (with confirmation); status line; uptime 24 h, 7 d, 30 d; TLS expiry; SVG response-time chart with range switch; incidents table; recent checks table; polling | F-04 | todo |
| F-08 | Settings page: Slack webhook (write-only field, shows configured state), Send test message, Save; account note | F-04 | todo |
| F-09 | Tests: Vitest for the fetch wrapper (CSRF, 401, problem+json), auth guard, list filtering and summary, form validation, chart scaling, duration formatting | F-03 to F-08 | todo |
| F-10 | Production packaging: multi-stage Dockerfile (build, then nginx unprivileged, SPA fallback, cache headers, runs as non-root, read-only root filesystem friendly), multi-arch; CI job (lint, typecheck, test, build, client up to date); enable the web image job in `release.yml` | F-01 | doing |
| F-11 | Docs: `web/README.md` (run, build, regenerate client), ledger sync | all | todo |

## Acceptance notes
- Every page works with the keyboard and uses semantic elements (labels tied to inputs, buttons for actions, tables for tabular data).
- No tokens or secrets in `localStorage` or `sessionStorage`; the session is the HttpOnly cookie.
- The Slack webhook URL is never displayed after saving.
- Time is shown in the browser's local time; ranges and durations are formatted consistently by one helper.
- Polling pauses when the tab is hidden (TanStack Query default) and shows a subtle "last updated" time.
