# Hub frontend (React SPA) — module diagram

Source of truth: `internal/webapp/frontend/src`. The built output is
committed at `internal/webapp/static` (the `go:embed` target), so `go build`
never needs Node. Reflects the code as of this commit; update this file in
any PR that changes these modules or their relationships.

```mermaid
classDiagram
    direction LR

    class ErrorBoundary {
        getDerivedStateFromError
        renders a page with a way back
    }
    note for ErrorBoundary "ErrorBoundary.tsx — the app's floor, mounted in main.tsx ABOVE QueryClientProvider so it covers every route. React unmounts the whole tree when a render throws and nothing catches it, and the address bar keeps the URL, so a reload reproduces the blank page: a permanent client-side DoS that another member's CONTENT can reach (a link in a teammate's markdown reaching decodePath, a folder named `constructor` reaching ProjectIcon). Deliberately the smallest thing that works — no reporting, no retry machine, no per-route boundaries"

    class App {
        mode from /api/config
    }
    note for App "App.tsx — picks HubApp (multi-project) or VolumeApp (single volume) from server config; frontend learns everything from the API, never sees storage or credentials"

    class HubApp {
        project list, org walls
        admin panels, invites
    }
    class VolumeApp {
        thin wrapper: one volume
    }
    class Browser {
        folder listing, file view
        per-view routes
    }

    class router {
        +VIEW_ROUTES dashboard history install settings
        +LEGACY_VIEWS insights to dashboard
        +top-level routes orgs billing
        +parseRoute(url, mode) Route
        +Route.version ?v= sha, one past version
        +Route.trailingSlash notes/ resolves, then replaces to notes
        +urlForPath(path, projectId, version)
        +urlForView / encodePath / decodePath
    }
    class nav {
        +navigate(url)
        +useLocationPath() pathname + search
        +linkProps(href)
        +Redirect
    }
    note for router "Two lookups on peer-authored path segments are now prototype-safe and one is throw-safe: legacyView() goes through Object.hasOwn, because LEGACY_VIEWS['constructor'] is truthy and turned a folder of that name into a view whose name was a FUNCTION; decodePath falls back to the raw segment instead of letting decodeURIComponent throw URIError out of a useMemo during render. Same shape as ProjectIcon's PROJECT_ICONS lookup in shell.tsx"
    note for nav "nav.ts + router.ts — deliberately NOT a router library (react-router v7 startTransition left stale views); History-API path routing, slashes literal, every user-facing page owns a URL path. A version is not a view route (the first segment after the project id is reserved for view names) — it rides as ?v=, so useLocationPath must snapshot the search too or the URL changes and nothing re-renders"

    class api {
        +getJSON / postJSON / api
        +getResponse (raw bytes)
        +PRODUCT_EVENTS method+path → event
        types.ts server contracts
    }
    note for api "api/http.ts — all URLs root-absolute so deep paths never break relative resolution. Every mutating call goes through api()/postJSON(), so one table there is the whole product-event surface: a new write is measured or it isn't, instead of depending on someone remembering a capture() call"

    class analytics {
        +initAnalytics(config)
        +track(event, props)
    }
    note for analytics "analytics.ts — posthog-js is fetched from the CDN at runtime, never installed: with no `analytics` in /api/config this module makes no request and the OSS bundle carries no tracker. capture_pageview history_change because the router is History-API. Replay masks every text node (maskTextSelector *) — in this product nearly all of it is customer file names and document bodies"

    class hooks {
        +useConfig
        +useHub
        +useBrowse
        +useTextAt (any URL) → useBlobText (sha-keyed, immutable)
    }
    note for hooks "TanStack Query wrappers over the viewer APIs. useTextAt fetches any URL and sniffs it — the Content-Length cheap-out lives here (HTTP), the byte decision in lib/sniff.ts (pure). A live path must not be cached immutable; a sha can be"

    class components {
        FileView FolderListing FileTree
        HistoryView HistoryRow DiffView VersionBanner
        Insights ShareDialog NewProjectDialog
        ShareBanner SharesTable AdminTable
        OrgAdmin HubSettings ProjectSettings
        Palette shell AccountBar ...
    }
    note for components "NewProjectDialog replaced ProjectNav's name-only modalPrompt: name + starting point, POSTing {name, template}. Its options come from useConfig()'s `templates`, never a hardcoded list, so a hub shipping another template needs no frontend change; &quot;Empty project&quot; (the empty value) stays preselected so an unpicked create behaves exactly as it did before templates. modal.tsx keeps its one-field API — teaching it about choices would tax every other caller"
    note for components "FileView's transformHTML now drops `data:image/svg` from any rendered img and any `data:` href from any rendered link — goldmark admits them, and an inline SVG is a document rather than a picture (the same property the server's sandboxInline walls off). Insights builds its per-device folder bag with Object.create(null), since folder names come off a peer's journal and one named __proto__ silently emptied the matrix. style.css sets unicode-bidi isolate-override on the peer-authored strings a reader is expected to CHECK (listing rows, breadcrumb, history path/note/device) — journal.SafeText refuses the bidi CONTROLS, but a single strong-RTL LETTER is legal and still reorders a row"
    note for components "components/ui — shadcn/ui primitives (Radix, copied in), themed from BearDrive tokens in tw.css; rendered markdown is transformed as a string before mounting, link clicks delegated on the container — never patch the dangerouslySetInnerHTML subtree"

    class lib {
        +diff.ts splitLines lcsDiff diffText
        +runs.ts groupRuns runFileCount
        +heat.ts heatFor heatTotal heatText heatLevel hotPathSplit
        +heat.ts ageRange isFlatRange ageSpanLabel (treemap scale)
        +sniff.ts sniffBytes BlobText MAX_BYTES
        +utils.ts
    }
    note for lib "pure, no React, unit-tested on node (npm test) — the line diff is ~40 lines, cheaper than auditing a diff package. heat.ts is the one read-count arithmetic: every surface (file header, folder listing, Dashboard bar) totals and splits through it, so they cannot disagree; useBrowse re-exports it"

    ErrorBoundary --> App : wraps the whole tree
    App --> HubApp
    App --> VolumeApp
    HubApp --> Browser
    VolumeApp --> Browser
    HubApp --> router
    Browser --> router
    Browser --> components
    HubApp --> components
    components --> nav : linkProps navigate
    components --> lib : diffText groupRuns hotPathSplit
    hooks --> lib : re-exports heat.ts, sniffBytes
    hooks --> api
    Browser --> hooks
    HubApp --> hooks
    hooks --> analytics : initAnalytics + identify on config
    api --> analytics : track(product event)
    Browser --> analytics : share_created (the one raw fetch)
```
