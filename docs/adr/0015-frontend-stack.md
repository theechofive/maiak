# ADR-0015: Frontend: minimal React + Vite, no design system

- Status: Accepted
- Date: 2026-10-20

## Context
The owner wants the simplest possible frontend for the MVP: pure UX, no visual design, usable from day one. The API is REST + OpenAPI on the same origin (ADR-0008).

## Decision
- React + TypeScript on Vite, React Router, TanStack Query (polling every 15 s, no WebSocket/SSE), client generated from `api/openapi.yaml` with `openapi-typescript` + `openapi-fetch`.
- Styling: Pico.css (classless defaults) plus a few lines of custom CSS. Response-time chart is hand-written SVG, no chart library.
- Served as static files by nginx (`ghcr.io/theechofive/maiak-web`, multi-arch), routed at `/` by the same HTTPRoute (`/api` goes to the api).
- Pages: login, monitors list, add/edit monitor, monitor details, settings (Slack). Wireframes were agreed in the design session; the structure is in `docs/tasks/frontend.md`.
- Tests: Vitest + Testing Library for the pages' logic; no end-to-end browser tests in the MVP.
- Auth is the session cookie plus the `X-CSRF-Token` header obtained at login or `/auth/me`; the client never stores tokens in `localStorage`.

## Consequences
- Fast to build and easy to replace; no design debt because there is no design.
- Accessibility is limited to semantic HTML and Pico defaults.
- Closes ledger L-015.

## Alternatives considered
- HTMX/server-rendered pages from Go: simpler still, but breaks the spec-first client generation and the same stack the owner planned.
- A component library (MUI, shadcn): design decisions the MVP does not need.
- Vue/Svelte: no advantage here over the already agreed React/TypeScript.
