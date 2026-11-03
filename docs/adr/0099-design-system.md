# ADR-0099: Design system and component library

- Status: Accepted
- Date: 2026-11-02
- Supersedes the styling part of [ADR-0015](0015-frontend-stack.md)

## Context
ADR-0015 chose Pico.css and "no visual design" so the MVP could ship. The product now needs its own look and a component library that people and agents build with. A design direction ("Instrument") was developed in Claude Design. Its tokens already target Tailwind v4 and shadcn naming.

Frontend decisions are owned by the frontend team. This record is a note for later readers, not a request for approval.

## Decision
- **Tailwind v4 plus our own components in `web/src/components/ui`.** No shadcn or Radix: after reading the kit, every component is simple enough for a native element and a few classes. Modal focus handling comes from the native `dialog`, arrow keys from native radios, the option list from native `select`. Radix can be added for one component if a real need appears. Runtime dependencies stay at zero, apart from the fonts.
- **Tailwind's defaults are removed** (`--color-*`, `--text-*`, `--shadow-*`, `--radius-*`, `--font-*`), so only design tokens generate utilities. An ESLint rule adds what Tailwind cannot see: raw form elements in pages and arbitrary colour values.
- **Theme by `light-dark()` and `color-scheme`**, following the system by default. Amended 2026-11-03: people asked where to switch, so Settings has System / Light / Dark, saved in `localStorage` per browser. `.theme-light` / `.theme-dark` on `html` force it; `public/theme.js`, a blocking same-origin script (the CSP forbids inline ones), applies the saved choice before the first paint, so there is no flash. Requires Chrome 123, Firefox 120, Safari 17.5 (2024), acceptable for this audience.
- **Fonts self-hosted** through `@fontsource-variable/instrument-sans` and `azeret-mono`, because the CSP is `font-src 'self'`. Interface language is English, so the Latin subsets are enough.
- **The gallery is a second entry point** (`web/ui-kit.html`), not a route. The dev server serves it, `vite build` does not include it, and it cannot affect the app. No Storybook.
- **The API is the source of facts; the mock-up only of the look.** Elements without real data are not built (`docs/design-system.md`, L-242).
- **Migration page by page.** The kit ships first without touching the app. The first migrated page switches `main.tsx` to `pico.conditional.min.css` and wraps pages not yet migrated in `.pico`, so both systems coexist; the last one removes Pico.

## Consequences
- The migration finished in F-27: Pico and `styles.css` are gone, and an ESLint rule (`web/eslint.config.js`) rejects raw form elements and arbitrary colours in pages. The CSP no longer allows inline styles (L-053).
- `ConfirmDialog` becomes truly modal, which will close L-051 when pages move to it. jsdom has no `showModal`, so `test-setup.ts` stubs it; real modality is checked in a browser.
- A list-row ribbon, per-segment p95 and similar ideas wait for the backend (L-242).

## Alternatives considered
- **shadcn/ui with Radix.** Recommended at first, dropped after reading the kit: more dependencies and code for components that native elements cover, and Radix's custom select is awkward in jsdom.
- **A new Pico theme.** Pico styles every tag globally; a square, dense instrument look would fight it everywhere.
- **Storybook.** A large dependency for what one gallery page does.
- **A `.dark` class.** Doubles the token list and needs a script before first paint.
