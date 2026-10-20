# web

React SPA for Maiak (ADR-0015): Vite, React, TypeScript (strict), React Router, TanStack Query, Pico.css. The API client is generated from `../api/openapi.yaml` (arrives with F-02).

Requires Node 22 (`.nvmrc`).

## Commands

```bash
npm ci               # install from the lockfile
npm run dev          # dev server on :5173, /api proxied to MAIAK_API_TARGET (default http://localhost:8080)
npm run build        # typecheck + production build into dist/
npm run preview      # serve dist/ locally
npm run lint         # eslint (flat config)
npm run typecheck    # tsc --noEmit
npm test             # vitest (jsdom + Testing Library)
npm run format       # prettier --write
```

## Layout

- `src/main.tsx` providers (Query, Router); `src/App.tsx` routes
- `src/pages/` one file per page (currently placeholders)
- `src/lib/` shared helpers (`format.ts`: duration formatting)
- `src/styles.css` the few custom CSS rules on top of Pico

## Production image

`Dockerfile` builds the SPA and serves `dist/` with `nginxinc/nginx-unprivileged` on port 8080 as a non-root user; the root filesystem can be read-only. `nginx.conf` provides:

- SPA fallback (`try_files $uri /index.html`); `/healthz` answers 200 from nginx itself
- `Cache-Control: immutable` (1 year) for hashed files in `/assets/`, `no-cache` for `index.html`
- security headers: `nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, and a CSP limited to same origin. `style-src` allows `'unsafe-inline'` because React inline `style` attributes (for example the SVG chart) are blocked otherwise; scripts stay `'self'` only.
- all nginx temp paths in `/tmp`, so only `/tmp` must be writable

```bash
docker build -t maiak-web web
docker run --rm --read-only --tmpfs /tmp -p 8080:8080 maiak-web
```
