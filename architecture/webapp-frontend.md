# Hub frontend (React SPA) — module diagram

Source of truth: `internal/webapp/frontend/src`. The built output is
committed at `internal/webapp/static` (the `go:embed` target), so `go build`
never needs Node. Reflects the code as of this commit; update this file in
any PR that changes these modules or their relationships.

```mermaid
classDiagram
    direction LR

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
        +useBlobText (sha-keyed, immutable)
    }
    note for hooks "TanStack Query wrappers over the viewer APIs"

    class components {
        FileView FolderListing FileTree
        HistoryView HistoryRow DiffView VersionBanner
        Insights ShareDialog
        ShareBanner SharesTable AdminTable
        OrgAdmin HubSettings ProjectSettings
        Palette shell AccountBar ...
    }
    note for components "components/ui — shadcn/ui primitives (Radix, copied in), themed from BearDrive tokens in tw.css; rendered markdown is transformed as a string before mounting, link clicks delegated on the container — never patch the dangerouslySetInnerHTML subtree"

    class lib {
        +diff.ts splitLines lcsDiff diffText
        +utils.ts
    }
    note for lib "pure, no React, unit-tested on node (npm test) — the line diff is ~40 lines, cheaper than auditing a diff package"

    App --> HubApp
    App --> VolumeApp
    HubApp --> Browser
    VolumeApp --> Browser
    HubApp --> router
    Browser --> router
    Browser --> components
    HubApp --> components
    components --> nav : linkProps navigate
    components --> lib : diffText
    hooks --> api
    Browser --> hooks
    HubApp --> hooks
    hooks --> analytics : initAnalytics + identify on config
    api --> analytics : track(product event)
    Browser --> analytics : share_created (the one raw fetch)
```
