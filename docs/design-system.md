# Maiak design system

Direction: **Instrument**. A precision instrument with a quiet soul: square corners, hairlines, tabular figures. Decision record: [ADR-0099](adr/0099-design-system.md). Rules for agents: [`web/CLAUDE.md`](../web/CLAUDE.md).

**See it:** `cd web && npm run dev`, then open <http://localhost:5173/ui-kit.html>. The gallery shows every component in every state, with a light and dark switch. It is served by the dev server only and is never in the production build.

## Principles
1. **Quiet by default.** A healthy product is almost silent. Up is a grey dot. Colour is earned: strong colour appears only where attention is needed, so one failure is visible from across the room.
2. **The filled red block means down.** It is the only filled colour block in the product. Nothing else may use it.
3. **Never colour alone.** Every state pairs a shape with a word: dot Up, triangle Down, two bars Paused, hollow dot Pending, diamond warning.
4. **Real data only.** The UI shows what the API returns. A mock-up is a suggestion: if the data does not exist, the element is left out and listed under [Needs the backend](#needs-the-backend).
5. **Honest about failure.** No "Copied" unless the clipboard accepted it. No "permanent" for a soft delete. Copy that states a consequence must be true of the backend.
6. **Motion is rare.** The refresh sweep marks new data; the slow beam on the sign-in screens is part of the brand. Nothing else moves at rest, both stop under reduced motion, and skeletons never shimmer.

## Using it
```tsx
import { Button, Field, Input, StatusIndicator, Table, Td, Th, Tr } from "../components/ui";

<Field label="Name" hint="Shown in alerts." error={errors.name}>
  {(control) => <Input {...control} value={name} onChange={(e) => setName(e.target.value)} />}
</Field>;
```

The stylesheet `web/src/components/ui/theme.css` is imported once, by the entry point. Style your own layout with Tailwind utilities and the tokens below. `className` on a kit component is for layout (margins, width, flex), never for its look.

### Tokens
Everything is a CSS variable with a Tailwind utility of the same name. Tailwind's default palette, text sizes, shadows and radii are **removed on purpose**: `bg-blue-500` and `text-sm` generate nothing.

| Group | Utilities | Use |
|---|---|---|
| Surface | `bg-background`, `bg-card`, `bg-popover`, `bg-field`, `bg-muted` | page, card, overlay, input, quiet fill |
| Text | `text-foreground`, `text-muted-foreground`, `text-subtle-foreground` | primary, secondary, tertiary |
| Lines | `border-border`, `border-border-faint`, `border-input`, `border-input-hover` | rules, inner rules, controls |
| Action | `bg-primary`, `bg-destructive` and their `-foreground`, `-hover`, `-active` | buttons |
| Signal | `text-down`, `bg-down-tint`, `border-down-border`; `text-warning`, `bg-warning-tint`, `border-warning-border`; `text-success` | down, warning, success |
| Data | `bg-ribbon`, `bg-inverse`, `text-inverse-foreground`, `bg-overlay` | ribbon, tooltips, dialog backdrop |

| Type | Size | Use |
|---|---|---|
| `text-display` | 32 | page title |
| `text-headline` | 24 | phone page title, metric figures |
| `text-title` | 18 | dialog title |
| `text-lead` | 15 | section title |
| `text-body` | 13.5 | default |
| `text-caption` | 12.5 | labels, hints, errors |
| `font-mono text-mono` | 11.5 | secondary mono: URLs, times |
| `eyebrow` | 10 | mono capitals: table heads, metric labels |

Mono is for machine values (URLs, figures, tokens); sans is for words. Radius is always zero. Elevation is a hairline; `shadow-overlay` is for things floating above the page (dialogs, toasts).

**Theme.** System by default; the sun and moon in the top bar (`ThemeToggle`, press the active one again to follow the system) and Settings → Appearance switch to Light or Dark for this browser (`src/lib/theme.ts`, `localStorage`). Colours are `light-dark()` pairs on `color-scheme`, so the switch is one class on `html` (`.theme-light`, `.theme-dark`), set before the first paint by `public/theme.js`. Force a subtree the same way (the auth screens are always dark). Add a colour as a `light-dark()` pair in `:root` and map it in `@theme inline`.

**Phone.** Below `sm` (640 px) controls grow to 44 px (small ones to 36 px) and inputs to 16 px text, because iOS zooms into anything smaller. Kit components do this themselves.

### Components
All from `web/src/components/ui`.

| Component | Notes |
|---|---|
| `Button`, `buttonClass()` | `primary`, `secondary`, `ghost`, `ghost-destructive` (red words for an action that confirms next), `destructive`, `link` (words inside a sentence or a one-line notice); `default`, `small`, `square` (page numbers); `busy`; `mono` for machine values. One primary per view. `buttonClass()` styles a router `Link`. Use `aria-disabled` plus a visible reason at a plan limit, so the button stays focusable. |
| `Field` | Label, hint, error, wired by id. Children get the props for the control. |
| `Input` | `mono` for machine values, `secret` to mask without a password manager prompt, `start`/`end` for fixed text such as `https://` or `s` (hidden from screen readers), `action` for a focusable button inside the field such as Show password. |
| `Textarea`, `Select` | `Select` is the native list on purpose. |
| `ReadOnlyValue` | A value that cannot change yet, such as the fixed 60 s interval. |
| `Checkbox` | With `label`, `description` and `error` (a server's answer about the choice). |
| `Segmented` | Native radios in one bar: arrow keys and screen readers work. For two to four short exclusive options. |
| `StatusIndicator` | `up`, `down`, `paused`, `pending`. `inline` is the unfilled down for sentences. |
| `Badge` | Mono capitals. A toned badge needs a shape or a word that carries the meaning. |
| `SignalRibbon` | See below. Takes the `series` of `GET /monitors/{id}/stats`. |
| `RibbonPlaceholder`, `RibbonSkeleton` | No data yet; loading. |
| `Sweep` | Bump `runKey` when data refreshes. Parent needs `relative overflow-hidden`. |
| `Metric`, `MetricStrip` | A ruled strip, never a row of cards. |
| `Meter` | Usage against a limit. Quiet until 80 %. |
| `Table`, `Th`, `Td`, `Tr` | Real table semantics. `numeric` right-aligns in mono. `Tr` has `tone` and `interactive`. |
| `Pagination` | Label, Previous, numbers up to seven pages, Next. |
| `Card` | A hairline. Prefer plain sections separated by rules; use a card only for a self-contained block. |
| `Alert` | A banner in the flow. `danger` interrupts screen readers; `warning` and `neutral` do not. |
| `EmptyState`, `ErrorState` | `ErrorState` has `page` and `section` variants; `section` keeps the layout when one part failed. |
| `Skeleton` | Static shapes. |
| `Loading` | A section is loading: a skeleton for the eye (one bar by default, or your own), the label for screen readers. |
| `EmptyNote` | A quiet one-line empty result: "No incidents." |
| `Toast` | Presentational. Timing and the live region stay in `ToastProvider`: successes leave after 5 s, never while the pointer is over the stack; errors stay until dismissed. |
| `Tooltip` | After 300 ms on hover and focus; adds to the child's own description. A reason that must be read belongs next to the control instead. |
| `CopyField` | One-time secrets. Reports a failed copy. |
| `AuthScreen` (`components/AuthScreen.tsx`) | The sign-in and registration frame: always dark (`.theme-dark`), one panel, the slow beam (`.maiak-beam`, still under reduced motion). |
| `WriteOnlyValue` | A stored secret that can only be replaced. |
| `ConfirmDialog` | Native modal `dialog`. Focus starts on Cancel, Escape cancels. State the real consequences. |
| `DotShape`, `TriangleShape`, `DiamondShape`, `PauseShape`, `HollowDotShape`, `Spinner` | The status shapes, for custom composition. They are `currentColor`: colour them with `text-*`, size them with `size`, never with `className` (utilities are not merged). |

### The Signal Ribbon
One mark for a monitor's history, replacing separate uptime bars and a latency chart. It maps directly to the API's `LatencyPoint`:

- **Height** is `avgMs` relative to the slowest bucket of the range (`ribbonScale`), with a 6 % minimum so a bucket never disappears. Buckets where at least 80 % of checks failed are full height and are left out of the scale, so one timeout does not flatten the rest.
- **Tone** is `1 - okRatio`: plain `--ribbon` with no failures, otherwise `color-mix(in oklab, ribbon, destructive (40 + 60 × failed)%)`. Oklab, because oklch would pass through violet.
- **A break** is `okRatio: null`: no check ran.
- **Inspect** with hover or Tab, then ← → Home End. The tooltip shows time, uptime and average. p95 exists per range, not per bucket, so show one p95 figure next to the ribbon.

The logic is in `web/src/lib/ribbon.ts`, with tests. Give it a `label` to make it interactive; without one it is decorative and hidden from assistive technology. Do not put a ribbon in a list row until the list endpoint returns the data (see below): one `/stats` call per row would scan raw results on every poll.

## Where the code follows the API, not the mock-ups
The Claude Design project ("Maiak Monitor Dashboard Directions") is the source of the look, not of the facts. Do not port these from it:

| Mock-up | Reality | Do this |
|---|---|---|
| Three locations, a location column | One location, `home-eu-central-1` | Leave out |
| `#ops-alerts` | Channels have a user-given name since S-01 (ADR-0100), not the Slack channel itself | Show `Channel.name` as typed |
| "Why" analysis, "HTTP 502 Bad Gateway", "Connection refused" | Only an error kind | `describeErrorKind()` |
| Starter/Team/Business plans, "See plans", member limits | `free` and `internal`, no plans page; the member limit (`max_members`, T-01) counts open invitations too | Names and limits from the API only |
| Alert after 1-10 | 1-3 | `Segmented` with 1, 2, 3 |
| Exact status 100-599 | 200-299, 400-599 | Existing `monitorForm.ts` rules |
| `404 no_service` from Slack | Slack's answer is never returned | A general message |
| Duplicate monitor name error, private IP shown in an error | Not server behaviour | Show the server's field errors |
| "This can't be undone", "permanent" | Monitors and tenants are soft deleted | Existing copy |
| Suspended tenant "sends no alerts", "pings not accepted" | Alerts wait; pings still answer 204 | Existing copy |
| "Copied" regardless of the result | The clipboard can refuse | `CopyField` |
| Pending shown as Up | `unknown` is Pending | `statusText()` |

Deviations in the kit itself, chosen on purpose:

- **`Select` is native**, not a custom listbox: it works on every phone and screen reader for no code.
- **One focus style**: a 2 px outline everywhere, including inputs, instead of the kit's inset border on inputs.
- **Type scale** consolidated from about sixteen sizes in the mock-ups to eight.
- **`light-dark()`** instead of a `.dark` class, so there is no theme script to run before first paint (`script-src 'self'` forbids an inline one).

## Roles in the UI
The server decides what a role may do (`internal/authz`, ADR-0102); the web mirrors it in `web/src/lib/roles.ts` only to avoid offering what would be refused. A control the role does not allow is left out, and one quiet line says why and who can do it (`ROLE_HINTS`), never a disabled control without a reason. Read-only views stay complete: a viewer sees every monitor, incident and channel. Secrets (heartbeat URLs, invitation links) are shown only to the person who just created them, in a `CopyField`, and live in component state only.

## Needs the backend
Good ideas from the mock-ups that the API cannot serve today. Not built; tracked as [L-242](LEDGER.md).

| Idea | Missing |
|---|---|
| Ribbon in monitor list rows | Segments in `MonitorListItem` (24 hourly ones from rollups would respect the rule that the list never scans raw results) |
| p95 per ribbon segment | `p95Ms` in `LatencyPoint` |
| "Paused 2 h ago", "Paused by" | `pausedAt`, actor |
| "Alert sent at 03:04" | Delivery time in the incident |
| "HTTP 503" in a list row | HTTP status in `MonitorListItem` |
| "Last 15 of 1,435", Older checks | A total and a cursor on `/checks` |
| Ribbon for watchdogs | A ping series |
| "Suspended on … by …", last activity, temporary-password state | Fields on `AdminTenant` and `AdminUser` |
| Users, monitors and limits per plan; tenants over their limit | Fields on `AdminPlanCount` and `AdminTenant` |
| Which member matched an admin search | The matched login in the search result |

## Enforced
`npm run lint` runs `eslint-plugin-jsx-a11y` everywhere and rejects, in every page outside `src/components/ui`: raw `<button>`, `<input>`, `<select>`, `<textarea>`, `<table>` and `<dialog>`, and arbitrary colours such as `bg-[#fff]`, `text-[oklch(…)]` or a colour value in `style` (`var(--token)` is fine). Tailwind's default palette is removed, so `bg-blue-500` generates nothing. The CSP allows no inline styles: computed values go through React's `style` prop, never a `<style>` tag.

## Adding to the system
1. Check the gallery first. A new component is justified only if a real page needs it twice.
2. Put it in `web/src/components/ui`, export it from `index.ts`, show it in `Gallery.tsx`, cover its behaviour in `ui.test.tsx`, add it to the table above.
3. Keep it presentational: no fetching, no routing, no business rules.
4. Use tokens only. If a token is missing, add it to `theme.css` for both themes.
