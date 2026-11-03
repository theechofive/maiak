# Web: how we build the frontend

This file is for the frontend (`web/`). Claude Code reads it when it works in this folder, on top of the root `CLAUDE.md`, which still governs everything outside `web/` (backend, API contract, deployment, migrations).

## Who decides

We, the frontend team, own every frontend decision: stack, libraries, design, components, copy. Decide it, write it down (an ADR for anything structural, the ledger for anything deferred), and move on; nobody outside signs it off. The API contract is not ours: a missing field goes to the backend list (L-242), never into a workaround.

## Where things are

- **Design system:** `docs/design-system.md` (tokens, components, the Signal Ribbon, what the API cannot serve yet). Decision record: ADR-0099.
- **Components:** `src/components/ui`; every one, in every state, in the gallery at <http://localhost:5173/ui-kit.html>.
- **Design source:** the Claude Design project "Maiak Monitor Dashboard Directions". It is the source of the look, not of the facts: mock-ups invent data.
- **Pure logic with tests:** `src/lib` (formatting, monitor state, the ribbon, form rules).

## How we work

1. **A branch per piece of work**, small vertical steps, each one leaves the app working.
2. **Real data first.** Before showing anything, find it in `api/openapi.yaml`. What the API lacks is left out and listed in "Needs the backend" in the design system doc and L-242.
3. **Look at it in a browser before calling it done**, in light and dark, and at 390 px. `npm run dev` proxies `/api` to `MAIAK_API_TARGET` (the cluster needs the VPN). For states the cluster lacks (down, paused, watchdogs), point it at a local mock API on fictional data; never test destructive actions against the cluster.
4. **Compare with the mock-up** side by side, then decide each difference: follow it, or keep ours and say why.
5. **Tests by role and label**, as a user finds things. A bug found in the browser gets a test.

## Rules

1. **Components come from the kit.** No raw `<button>`, `<input>`, `<select>`, `<textarea>`, `<table>` or `<dialog>` in a page. If the kit lacks something, add it there (checklist in the design system doc). ESLint enforces this; a deliberate exception needs `eslint-disable-next-line <rule> -- <why>`.
2. **Tokens only.** No hex, `rgb()`, `oklch()` or other colour values in `className` or `style`; `var(--token)` is fine. Inline `style` is only for values computed from data, such as a bar's height. Tailwind's default palette is removed, so `bg-blue-500` silently generates nothing. ESLint enforces this too.
3. **`className` on a kit component is for layout** (margin, width, flex, visibility), never its look. Utilities are not merged: two that set the same property fight, and the stylesheet order, not the class order, picks the winner. Need another look? Add a variant or a prop to the kit (`variant="link"`, `mono`, `size="square"`).
4. **Copy must be true of the backend.** Soft delete is not "permanent". Never report a success that did not happen; a paused monitor is not a new one.
5. **Status is a shape and a word**, through `StatusIndicator`. The filled red block means down and nothing else. Healthy stays grey; amber is for warnings and suspensions.
6. **Every data section has loading, error and empty states** (`Loading`, `ErrorState`, `EmptyNote`), and one section's failure must not break the page.
7. **Keyboard and screen reader first.** Every control works from the keyboard, focus goes where the user's next step is, and accessible names stay stable. `eslint-plugin-jsx-a11y` runs with the lint. No `data-testid`.
8. **Motion is rare and purposeful:** the refresh sweep after new data, and the beam on the sign-in screens, which is part of the brand. Nothing else moves at rest, and everything stops under reduced motion.
9. **Spec-first.** The API client is generated: change `api/openapi.yaml`, then `npm run gen:api`. Never edit `src/api/generated`.
10. **Security stays as it is:** no inline styles or scripts (the CSP is `style-src 'self'`, `script-src 'self'`), no tokens in storage, secrets shown once and kept only in component state.

## Before finishing

`npm run lint && npm run typecheck && npm test && npm run format:check && npm run build`, then the browser check above. Update the design system doc for a new component or rule, and the ledger for anything deferred.
