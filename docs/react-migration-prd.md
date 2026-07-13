# PRD: Migrate the bdrive web frontend to React + TypeScript

This is the authoritative spec for rewriting `internal/webapp/static/`
(vanilla JS: `app.js` ~2,150 lines, `style.css`, `index.html`) as a React +
TypeScript app. Implement the phases in order; a phase is done only when
every acceptance box in its checklist is checked. Record progress and
blockers in §Status at the bottom.

## Goals

- Feature-for-feature parity with the current SPA — same routes, same
  behaviors, same visual design (port `style.css` verbatim; do not redesign).
- Maintainable component/hook structure replacing the 90-function single file.
- Keep the product's single-binary story fully intact.

## Non-goals (do NOT do these)

- No Go API changes. No new endpoints, no changed response shapes.
- No visual redesign, no CSS framework, no CSS modules — global stylesheet.
- `/auth/*` pages and `/s/<token>` share pages stay server-rendered Go.
- Markdown stays rendered server-side (`/api/.../render`); the frontend
  injects the returned HTML. Never add a client-side markdown renderer.
- No SSR, no Next.js. Vite static build only.

## Hard invariants (violating any of these fails the phase)

1. **`go build ./...` must work with no Node installed.** Built assets are
   committed at `internal/webapp/static/` (the existing `go:embed static`
   target in `internal/webapp/server.go`). Frontend source lives in
   `internal/webapp/frontend/` and `vite build` writes to `../static/`.
2. **Routing semantics unchanged**: native History-API paths —
   `/<project-id>/<url-encoded path>` (hub), `/<path>` (volume mode),
   `/<project-id>/insights`, `/<project-id>/history`, `/join/<token>`. No
   hash routing; slashes stay literal; all API/asset URLs root-absolute.
   Reserved prefixes `/api/`, `/auth/`, `/s/` must still 404 rather than
   fall back to the SPA shell (server behavior — keep it).
3. **Storage-blind + privacy**: the frontend learns everything from
   `/api/config` (+ `/api/projects` in hub mode). Never surface storage
   details; never expect or display human actor emails from the heat API.
4. **Never commit `internal/webapp/manual_serve_test.go`** (untracked local
   demo harness). Check `git status` before every commit.
5. Runtime deps allowed: `react`, `react-dom`, `react-router-dom`,
   `@tanstack/react-query`. Anything beyond these requires updating this
   PRD with a justification first. Dev deps: `vite`, `typescript`,
   `@vitejs/plugin-react`, `@playwright/test` (+ types).

## Toolchain & layout

```
internal/webapp/frontend/        # source (committed)
  package.json  vite.config.ts  tsconfig.json  index.html
  src/          # components, hooks, api types
  e2e/          # Playwright parity suite + hub harness
internal/webapp/static/          # vite build output (committed, generated)
```

- Vite `build.outDir: ../static`, `emptyOutDir: true`, hashed asset
  filenames under `static/assets/`.
- Dev: `vite dev` with `server.proxy` for `/api`, `/auth`, `/s` →
  `http://localhost:8080`.
- Freshness: add `internal/webapp/frontend/check-dist.sh` (npm ci && npm
  run build && `git diff --exit-code ../static`) and document it in
  CLAUDE.md as a pre-release step.
- TS API types: hand-write `src/api/types.ts` for the JSON shapes of
  `/api/config`, `/api/projects`, `tree`, `heat`, `history`, orgs, shares,
  admin. Derive them from the Go handler structs in `internal/webapp/`.

## Server-side changes (the ONLY Go changes allowed)

- `server.go frontend()`: serve `assets/*` (hashed filenames) with
  `Cache-Control: public, max-age=31536000, immutable`; keep `no-cache` for
  `index.html`/everything else. Adjust the embedded-asset test if any.
- A committed e2e hub harness (see §Verification).

## Component map (port targets)

| Current (app.js) | React module |
|---|---|
| boot, loadProjects, loadOrgs, acceptInviteFromURL | `App.tsx`, `useConfig`, `useProjects`, `useOrgs`, `InviteAccept` |
| selectProject, updateOrgBar, updateAdminBar, sidebar DOM | `Layout`, `Sidebar`, `OrgBar`, `AdminBar` |
| parseRoute/applyRoute/pushURL/urlFor* | React Router routes + `usePathRoute` helper (path encoding per current encodePath/decodePath) |
| refreshTree, renderNode/renderChildren, expandTo, applyTreeExpansion, markActive, revealInTree | `FileTree` (+ expansion state persisted as today) |
| openFolder, renderFolderListing, renderFolderHistory | `FolderListing` |
| openFile, showMeta, fixLinks, openWikilink | `FileView` (dangerouslySetInnerHTML on server HTML + link-rewrite effect that re-runs on content change) |
| setCrumb | `Breadcrumbs` |
| refreshHeat, heatFor/heatText/heatLevel | `useHeat` + heat dot components |
| showProjectHome, renderConnectGuide, guideSteps, GUIDE_AGENTS | `ProjectHome`, `ConnectGuide` (tabs; localStorage key `bdrive-guide-agent`; stale value falls back to first tab; pre-filled origin + project id) |
| showInsights, renderInsights, squarify, insightsTreemap/Matrix/Chart, renderHotPath, staleColor | `Insights` + SVG components (port math as-is) |
| showHistory, historyEntryRow, initHistory | `HistoryView` |
| showOrgAdmin, showHubSettings, showPending | `OrgAdmin`, `HubSettings`, `PendingApprovals` |
| modalPrompt/modalConfirm, toast, showShareDialog, updateShareButton | `Modal`, `Toaster`, `ShareDialog` |
| initUpload, uploadFile, sha256Hex | `UploadDropzone` + `useUpload` (init→content→commit flow, presign or relay) |
| showEmptyState, createProject | `EmptyState` |
| scrollMemo/pendingScroll/rememberScroll | per-route scroll restoration hook |

Server state through TanStack Query (poll interval from `/api/config`
refresh; invalidate after uploads/renames/admin actions — mirror today's
`refreshAll`). Reuse the SVG icon sprite from the current `index.html`.

## Phases

### Phase 0 — scaffold & pipeline
- [x] Vite+React+TS workspace at `internal/webapp/frontend/`; build lands in
      `internal/webapp/static/` and is committed. The branch's `static/` is
      the React build from Phase 0 on (the old files live on `main` /
      `git show main:internal/webapp/static/app.js` for porting reference);
      the parity gate in Phase 5 is what makes the branch mergeable.
- [x] `style.css` ported verbatim as global stylesheet; SVG sprite ported
      (kept inline in `index.html`, as before).
- [x] Dev proxy works against a local hub (`BDRIVE_DEV_PROXY` overrides the
      `localhost:8080` default).
- [x] Cache-header change in `frontend()` + assets served hashed
      (covered by `TestFrontendSPAFallback`).
- [x] `check-dist.sh` works.
- [x] e2e harness committed (`e2e_serve_test.go`, gated by
      `BDRIVE_E2E_SERVE=1`, port 8993, fresh state each run) and wired as
      Playwright's webServer; 4 shell specs green. Also ported in Phase 0
      (the e2e login flow needed it): the `getJSON`/`postJSON` layer with
      401→login redirect, `/api/config` boot, and the title/vault-name
      wiring (`src/api/http.ts`, `src/hooks/useConfig.ts`).
- [x] `go build ./...`, `go vet ./...`, `go test ./...` green.

### Phase 1 — shell: boot, session, projects, routing
- [ ] Boot from `/api/config`; volume vs hub mode both render.
- [ ] Project list + selection; project color chips (port `projColor`).
- [ ] Empty state + create project; `/join/<token>` invite accept.
- [ ] Deep link + refresh on every route resolves (SPA fallback).
- [ ] Sign-out link, admin bar, org bar render per session flags.

### Phase 2 — file browsing (long pole)
- [ ] Tree with expansion persistence, active marking, reveal-in-tree.
- [ ] Folder listing incl. heat dots (members) + folder history strip.
- [ ] File view: server-rendered markdown injected; wikilinks + relative
      links rewritten (`fixLinks` semantics); meta/provenance line.
- [ ] Breadcrumbs; per-route scroll restoration.
- [ ] Download + raw file view; share button state; share dialog.
- [ ] Drag-drop upload (presign and relay paths) with refresh after commit.
- [ ] Command palette (⌘K: file names, projects, actions — port the
      palette overlay from the old shell) and the topbar overflow menu
      (`more-btn`/`more-menu`) for narrow viewports.

### Phase 3 — project home, insights, history
- [ ] Project home at `/<pid>`: connect guide (3 tabs: "Claude Code &
      Cowork" plugin flow, Hermes CLI, Codex CLI; copy buttons; persisted
      tab; commands pre-filled with hub origin + project id).
- [ ] Insights embedded below the guide for admins/org-owners only
      (`canSeeInsights` logic), plus dedicated `/insights` route.
- [ ] Insights views: treemap (squarify), device matrix, chart, hot-path
      list — visually equivalent.
- [ ] History view: `?path=`/`?prefix=` modes, newest-first entries, blob
      version links, device attribution.
- [ ] Vault-name click returns to project home.

### Phase 4 — admin surfaces
- [ ] Org admin: rename, members (role change/remove), invite create/list/
      revoke with expiry, org shares list + revoke.
- [ ] Hub settings + pending-approval queue (approve/deny), policy toggles.
- [ ] All actions confirm via modal where the current UI does; toasts on
      success/error.

### Phase 5 — parity gate & swap
- [ ] Port the 17 checks from the pre-existing smoke suite (see
      §Verification) into `frontend/e2e/` as Playwright tests; extend with:
      login→browse→open markdown file, upload roundtrip, share create/
      revoke, org admin invite flow, history view, 404 on `/api/nope`.
- [ ] Full e2e suite green against the harness hub.
- [ ] Old `app.js`/old `index.html`/old `style.css` gone from the repo
      (replaced by build output); `git grep renderConnectGuide` finds only
      frontend/src.
- [ ] Docs updated: README (frontend dev section), CLAUDE.md (`webapp`
      package description — replace "dependency-free vanilla JS" with the
      React/Vite reality, build + check-dist instructions),
      `plugin/skills/beardrive/SKILL.md` only if it mentions frontend
      internals (it likely doesn't — verify).
- [ ] `go build ./...` from a clean checkout with no Node succeeds and
      serves the React app.

## Verification (every phase)

1. `cd internal/webapp/frontend && npm run build` — clean.
2. `go build ./... && go vet ./... && go test ./...` — green.
3. Playwright checks for all surfaces finished so far.

**e2e harness** (build in Phase 0, commit it): `frontend/e2e/hub.mjs` —
builds `bdrive` (`go build -o /tmp/... ./cmd/bdrive`), then starts it with a
scratch `BDRIVE_HOME`, hub mode on `file://<scratch>/storage`, uploads
enabled, builtin auth with one pre-seeded account (write the auth users_db
JSON directly, or reuse the approach in the untracked
`manual_serve_test.go` — programmatic seeding — in a small committed Go
helper under `internal/webapp/` guarded by an env var, e.g.
`BDRIVE_E2E_SERVE=1 go test -run TestE2EServe`). Seed a handful of files
(markdown with wikilinks, nested dirs, one binary) and some read-heat data.
Wire it as Playwright's `webServer`. Port: 8993. Test account:
`e2e@example.com` / a fixed password.

The 17 existing parity checks (from the previous smoke suite) cover:
landing URL is `/<pid>` (no insights redirect); 3 guide tabs in order;
one active tab; claude tab installs plugin (marketplace add + install);
claude tab `/beardrive:install connect to <origin>, project <pid>`; claude
tab has no raw CLI; claude tab mentions Cowork; stale saved "cowork" tab
falls back safely; codex tab full CLI flow (brew, login origin, init
--project, hooks install --agent codex); tab choice persisted in
localStorage; insights embedded on home for admins; guide renders above
insights; dedicated `/insights` route still works; vault-name click goes
home; browser back/forward across home↔file↔insights; reload on a deep
file path; reload on `/insights`.

## Git conventions

- Branch: `feat/react-frontend` off `main`. One PR at the end; do not
  merge/deploy/push to `main`.
- Commit per phase (more is fine), message prefix `feat(webapp): [phase N]`.
- Never commit: `manual_serve_test.go`, `node_modules/`, scratch dirs.
  Add `.gitignore` entries in `frontend/` for node_modules etc.

## Status

- [x] Phase 0
- [ ] Phase 1
- [ ] Phase 2
- [ ] Phase 3
- [ ] Phase 4
- [ ] Phase 5

Blockers / deviations: (record here; stop rather than deviate silently)
