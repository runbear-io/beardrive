# Spec: Mobile layout quality for the BearDrive web app

Authoritative spec for the mobile-polish goal loop. The bar is met by
independent review: a `beardrive-designer` subagent audits the running app
and returns scores; the implementing agent never scores its own work.

## Harness

Seeded hub: `BDRIVE_E2E_SERVE=1 go test -count=1 -timeout 8h -run
TestE2EServe ./internal/webapp` → http://localhost:8993 (state resets per
start). Accounts (password `e2e-pass-1` for all): `e2e@example.com`
(admin — insights, admin bar, org Manage), `member@example.com` (member),
`solo@example.com` (no org — onboarding empty state).

## Viewports (all must pass)

| Name | Size |
|---|---|
| phone-small | 360×800 |
| phone | 390×844 |
| phone-landscape | 844×390 |
| tablet | 768×1024 |

## Surfaces to audit (per viewport)

1. Login page (`/auth/login`) — server-rendered, still in scope.
2. Project home: connect guide (tabs, code blocks + copy buttons) and, as
   admin, the embedded Insights below (treemap/scatter/hot-path/matrix
   SVGs must not overflow or become unreadably small).
3. Folder listing (`/<pid>/notes`): rows, heat dots, Recent changes feed.
4. Markdown file view (`/<pid>/index.md`): content, breadcrumbs, meta
   line, topbar actions (Share/History/Upload/Download vs the ⋯ menu —
   targets must stay tappable, header must not wrap or overflow).
5. History (`/<pid>/history`): entry rows, expandable notes.
6. Dedicated Insights (`/<pid>/insights`).
7. Org admin panel (Manage) and hub settings (Admin) — lists, selects,
   buttons, toggles.
8. Off-canvas sidebar: hamburger open/close, backdrop, tree interaction,
   auto-close after selecting a file.
9. ⌘K palette (on tablet; on phones verify the Search button opens it and
   it is usable).
10. Modals (new-project prompt, share dialog, confirms) and toasts.
11. Onboarding empty state (as `solo@`).

## Scoring rubric (designer subagent returns 0–10 per category)

- **layout** — nothing overflows the viewport; no horizontal page scroll;
  wide content (code blocks, tables, SVGs, URLs) scrolls in its own box.
- **readability** — type sizes/line lengths sane; nothing truncated
  without recourse; contrast preserved.
- **tap-targets** — interactive elements ≥ ~44px effective target; no
  overlapping/cramped controls.
- **navigation** — sidebar, breadcrumbs, back/forward, deep links all
  usable one-handed; nothing reachable only by hover.
- **polish** — spacing, alignment, safe-area behavior, orientation change.

Findings carry severity (high/medium/low), the viewport+surface, and a
concrete CSS/markup fix suggestion.

## Exit bar

Two CONSECUTIVE designer rounds with every category ≥ 8/10 and zero
high-severity mobile findings across all viewports. Desktop (1360×900)
spot-checked each round — no regressions introduced by mobile fixes.

## Rules

- Fix in `internal/webapp/frontend` (prefer `src/style.css`; markup only
  when CSS can't). Rebuild committed static (`npm run build`) after every
  change; `npm run e2e` (42 specs) must stay green each iteration.
- No new runtime dependencies; no desktop redesign — mobile fixes only.
- Never commit `internal/webapp/manual_serve_test.go`.
- Branch `feat/mobile-polish`, commit per iteration
  (`feat(webapp): [mobile] ...`), PR at the end; never merge or deploy.
- Disputed/won't-fix findings: record below with reasoning, count them
  out of the exit bar only if justified here.

## Scorecard (append one row per designer round)

| Round | layout | readability | tap-targets | navigation | polish | high-sev findings |
|---|---|---|---|---|---|---|

## Won't-fix / disputed

(none yet)

## Status / blockers

(record and stop rather than deviate)
