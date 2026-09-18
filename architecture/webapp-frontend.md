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

    class shareMermaid {
        <<second Vite entry>>
        src/share-mermaid.ts → static/share-mermaid.js
        picks DARK/LIGHT from prefers-color-scheme
    }
    note for shareMermaid "The only script the server-rendered /s/ share page ever loads, and only when shares.go finds a mermaid fence in the document. Built as a SECOND rollup input with a fixed, unhashed name at the static root — sharedMarkdownShell is a Go const and cannot know a content hash, and server.go marks assets/ immutable for a year, so an unhashed file there would pin a stale bundle in shared caches. Mermaid's own chunks keep their hashed assets/ names, which is why the share page needs the ACAO header: its sandbox origin is opaque"

    class App {
        mode from /api/config
    }
    note for App "App.tsx — picks HubApp (multi-project) or VolumeApp (single volume) from server config; frontend learns everything from the API, never sees storage or credentials"

    class HubApp {
        project list, org walls
        admin panels, invites
        remembers last opened project (localStorage)
        +desktop mode: /setup/* routes, no project id
    }
    note for HubApp "One codebase serves the hub and the desktop sidecar; `desktop: true` from /api/config is the only switch. In desktop mode HubApp routes the project-less /setup, /setup/connect, /setup/syncing and /setup/done frames the same way it already routes /join/<token>, and Browser gains a Copy-web-link item built from the mount's own hub URL"

    class Setup {
        onboarding frames
        inspect folder → connect → syncing → done
        name from the URL, not the first poll
    }
    note for Setup "components/Setup.tsx — desktop-only. Every frame owns a URL, so reload and Back behave; the Syncing frame reads ?name= so its heading is right on the FIRST paint rather than reading 'Syncing your folder' until the 400ms status poll lands. That poll navigates away on a terminal phase, which is what makes the frame order-dependent under test"
    class VolumeApp {
        thin wrapper: one volume
    }
    class Browser {
        folder listing, file view
        per-view routes
        +moved: /resolve?path= on a tree miss only
        +scroll restoration: contentRef, memo, goal from lib/scroll
        +fullscreen: body.full-view from route.full, Exit / Esc / Back
    }
    note for Browser "Fullscreen is a QUERY PARAM on the file route (?full=1), for the reason ?v= is one: the first segment after the project id is reserved for view names, and a fullscreen file is the same page with different chrome. Two things the code forced: routeKey is withoutFull(useLocationPath()), because a ?full=1 push otherwise looks like a fresh route and arms a scroll goal of 0 — the reader is thrown to the top of the document the moment they ask for more of it; and the chrome is HIDDEN by a body class, never unmounted, so &lt;article id=content&gt; survives the toggle and keeps its scrollTop in both directions. display:none also takes the hidden controls out of the tab order and the accessibility tree together, which is what syncSidebarInert already encodes for the off-canvas sidebar. The Exit control is rendered by AppShell OUTSIDE the topbar it hides and painted over the content: the HTML sandbox is an opaque origin and the PDF viewer is the browser's own, so Esc can never reach the app from inside either one (BEA-195)"
    note for Browser "A missing path is decided from /tree alone — the file is never fetched — so the X-Bdrive-Canonical-Path header /file answers with would never reach the browser, and a moved FOLDER has no content fetch to hang a header on. The not-found branch asks GET /resolve?path= instead, then replaceState-navigates to the destination and prints one Moved from … line above it (BEA-81)"

    class router {
        +VIEW_ROUTES dashboard history install settings
        +LEGACY_VIEWS insights to dashboard
        +top-level routes orgs billing connections
        +parseRoute(url, mode) Route
        +Route.version ?v= sha, one past version
        +Route.full ?full=1, the file page with the chrome hidden
        +Route.editing /edit/&lt;path&gt;, the file page with the editor open
        +Route.trailingSlash notes/ resolves, then replaces to notes
        +Route.queryTarget history ?path= / ?prefix= resolves, then replaces to /history/target
        +Route.filters q user since until, history feed
        +historyFilterQuery(filters) / hasHistoryFilters
        +urlForPath(path, projectId, version, full, editing)
        +withoutFull(url) the same URL minus full — the scroll memo key
        +urlForView(view, projectId, target, filters) / encodePath / decodePath
        +projectByName(projects, seg) id, only when exactly one name matches
    }
    class McpConnections {
        the grants this account holds
        client name, projects, last used
        Disconnect → DELETE /api/mcp/grants/:id
    }
    note for McpConnections "components/McpConnections.tsx — the browser half of MCP. CONNECTING is not here: it happens on the server-rendered /oauth/authorize consent screen, which has to work before any grant exists and is where a human decides what an agent may touch. This page is seeing and revoking. Account-level, not per-project, so HubApp's stale-route guard has to name route.connections alongside route.org and route.billing — a top-level route missing from that condition is silently redirected to the last-opened project id"

    class nav {
        +navigate(url)
        +useLocationPath() pathname + search
        +linkProps(href)
        +Redirect
    }
    note for router "Two lookups on peer-authored path segments are now prototype-safe and one is throw-safe: legacyView() goes through Object.hasOwn, because LEGACY_VIEWS['constructor'] is truthy and turned a folder of that name into a view whose name was a FUNCTION; decodePath falls back to the raw segment instead of letting decodeURIComponent throw URIError out of a useMemo during render. Same shape as ProjectIcon's PROJECT_ICONS lookup in shell.tsx"
    note for router "Route.editing is the odd one: a path segment (/&lt;project-id&gt;/edit/&lt;path&gt;) for a thing that is not a view. The editor is the FILE page with a different surface, so `path` keeps carrying the file and nothing in Browser has to learn about editing — but it is a path rather than a ?flag because everyone editing one file shares a co-editing room, which makes the URL the INVITATION: paste it and the recipient is in the document with you. It is parsed before the view names, so /edit/history/notes.md edits a file under a folder called history, and urlForPath never emits the prefix without a path (the trailing-slash redirect would rewrite /edit/ to itself forever)"
    note for router "projectByName is what makes /wiki reach the project called wiki: the id never appears in the UI as something to copy, so a hand-typed first segment is the NAME the sidebar shows. It decodes the segment (route.project is the still-encoded slice) and returns an id only on EXACTLY one case-insensitive match — ProjectDB names are scoped per organization, so a viewer in two orgs can hold two projects named wiki and guessing between them is worse than the not-found page (BEA-140)"
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
        +useProjectEvents (one SSE per BROWSER → invalidate)
        +usePresence (10s heartbeat)
        +useTextAt (any URL) → useBlobText (sha-keyed, immutable)
        +fetchBlobText(url) BlobText
        +fileURLFor(apiBase, path, version) string
    }
    note for lib "collab.ts is the Yjs provider: SSE down, POST up, over the same pair /events already uses — no websocket dependency, no upgrade handshake, nothing special asked of a proxy that already carries the change stream. Only the client the hub calls `seed` builds the document from the file text; everyone else rebuilds from the relay log, because two independent seeds of the same text are two DIFFERENT Yjs documents and merging them duplicates every character. Awareness is posted separately and never logged. peerCount() is what tells a co-editor's snapshot (already in my buffer) from an outside write (a CLI, another device), which is the only case the peer-wrote banner should fire for"
    note for hooks "usePresence beats every 10s with the path you are on and renders the roster the hub pushes back — the roster ARRIVES on useProjectEvents' stream (onPresence), not on the POST, which is used only for first paint so a tab opening into a quiet project still sees who is there. One EventSource carries both frame types: presence invalidates nothing. The path is read through a ref so navigating does not tear the timer down, and the unmount beat sends leave:true as courtesy — the 15s TTL is the real guarantee. Every failure is swallowed: presence is decoration and must never surface an error"
    note for hooks "useProjectEvents is the one non-polling source: an EventSource on {apiBase}events whose frames invalidate exactly what a peer's write touched (tree/history/heat always; render per named path; text wholesale). It is what makes an OPEN file update at all — useTextAt has no refetchInterval, so before this a body fetched once stayed on screen until the reader navigated away. A resync frame, a truncated path list, or an unparseable frame all fall back to invalidating every body rather than guessing. Errors are deliberately silent: EventSource retries itself and a 5-minute tree refetch is still underneath, so a hub restart or a sleeping laptop must not write a log line. That interval used to be 15s, for tree AND heat AND projects — belt-and-braces from before this stream existed, and on a real project it re-sent the whole 1.65 MB tree four times a minute to a client that already knew nothing had changed. What remains is insurance against a stream that dies quietly on a tab nobody touches, not a freshness mechanism (docs/network-efficiency-prd.md)"
    note for hooks "ONE stream per browser, not per tab. Every tab of a project gets the identical fan-out, so tab two onward cost a slot at both ends for nothing: a permanently in-flight request against the hub's per-instance concurrency, and one of the browser's ~6 per-origin HTTP/1.1 sockets — which is how six tabs wedged the whole app, not just live updates. One tab holds the EventSource and relays each frame verbatim over a BroadcastChannel keyed per project; followers run the same handler on the same raw data string. Leadership is a Web Lock held for the leader's lifetime, so the browser reassigns it when that tab dies — a crash or force-quit included, which is the case a heartbeat-and-TTL scheme gets wrong. Frames lost in the handover gap are the poll's job, as they always were. Web Locks needs a secure context, so a plain-http LAN hub falls back to a stream per tab. /collab is deliberately NOT shared: two tabs editing one document are two distinct CRDT peers with their own awareness state"
    note for hooks "TanStack Query wrappers over the viewer APIs. useTextAt fetches any URL and sniffs it — the Content-Length cheap-out lives here (HTTP), the byte decision in lib/sniff.ts (pure). A live path must not be cached immutable; a sha can be. fileURLFor is the one definition of a file page's byte URL, so Copy and FileView cannot drift; fetchBlobText is exported for Copy, which must NOT read the shared text cache — TextView stores a string there and SniffView a BlobText"

    class components {
        FileView FolderListing FileTree
        HistoryView HistoryRow HistoryFilters DiffView VersionBanner ConflictBanner
        PresenceBar Editor VisualEdit
        Insights ShareDialog NewProjectDialog
        ShareBanner SharesTable AdminTable
        OrgAdmin HubSettings ProjectSettings
        Palette shell AccountBar ...
    }
    note for components "NewProjectDialog replaced ProjectNav's name-only modalPrompt: name + starting point, POSTing {name, template}. Its options come from useConfig()'s `templates`, never a hardcoded list, so a hub shipping another template needs no frontend change; the initial selection is options[0].value — the same array element the RECOMMENDED badge indexes, so the badged row and the checked row are one row by construction (on a template-less hub that row is 'I already have a folder', which still creates an empty project). modal.tsx keeps its one-field API — teaching it about choices would tax every other caller"
    note for components "HistoryFilters drives the SERVER (?q=/?user=/?since=/?until= on the history API), never the loaded page — filtering what is on screen would lie about everything below the fold and break next_cursor. Its state is Route.filters, so a narrowed feed is linkable, survives reload, and Back undoes it; the author list accumulates across fetches, because filtering by one author leaves only their rows loaded"
    note for components "FileView's transformHTML resolves the server's `wiki:` marker against flatFiles into a real urlForPath() href (unresolvable ones lose the href and get .wiki-missing), so copy-link/middle-click/new-tab work and only a plain click reaches the delegated handler — resolution used to happen at click time, which left a dead `wiki:guide` string in the DOM (BEA-136). It also drops `data:image/svg` from any rendered img and any `data:` href from any rendered link — goldmark admits them, and an inline SVG is a document rather than a picture (the same property the server's sandboxInline walls off). Insights builds its per-device folder bag with Object.create(null), since folder names come off a peer's journal and one named __proto__ silently emptied the matrix. style.css sets unicode-bidi isolate-override on the peer-authored strings a reader is expected to CHECK (listing rows, breadcrumb, history path/note/device) — journal.SafeText refuses the bidi CONTROLS, but a single strong-RTL LETTER is legal and still reorders a row"
    note for components "HistoryView's RunGroup header carries the run-wide undo (POST undo-run, gated by the same write permission as the per-row restore/remove). It asks the SERVER for the file list first (preview: true) rather than deriving it from the loaded feed — that window is paged and filterable, so a client-computed list is wrong exactly when the run is old. modal.tsx's Confirm.message widened from string to ReactNode for it (the prompt's one-field API is untouched), so the dialog can show every path, its action, and the &quot;changed after this run&quot; warning inline"
    note for components "FileView's MarkdownView renders SecretBadge above the content when the render response carries findings — VersionBanner's shape (a strip, role=status, no actions), the red family rather than the accent because accent+glow already means 'you are looking at an old version' and the two strips stack on the ?sha= view. It phrases from lib/secrets so the badge and the share dialog name the rule and the line identically"
    note for components "VisualEdit is click-to-edit for a synced HTML file: the page is rendered by the server's ?edit=1 view inside the SAME sandboxed iframe reading uses, and the editor is injected into it as a separate bundle (src/inline-edit.ts, built IIFE by vite.inline-edit.config.ts — an opaque-origin iframe cannot load a module script without CORS the hub has no business growing). Nothing here serializes the document: the iframe reports ONE element's inner HTML and the source range it belongs to, and this splices that range into the shared Y.Text, which is why the rest of the file survives byte-for-byte. Those ranges are held as Y.RelativePosition, never offsets — a peer's edit earlier in the file moves every number — and they are anchored against the CRDT ITSELF, never openSharedFile.current(), whose seed fallback reports a full document while the Y.Text being measured is empty and collapses every anchor onto index 0 (one edit then replaced an entire file). A patch whose resolved range would swallow a document the stamped range was only part of is refused outright. The iframe does NOT debounce its patch: for 700ms the edit lived only inside it, and Done tears the iframe down — typing and pressing Done is what finishing an edit looks like, and it silently lost the text"
    note for components "FileView's HtmlView re-mounts its iframe on the change stream. An iframe loads once, so leaving the editor rendered the file as it was when Done was pressed — BEFORE the save landed — and then sat there with the edit saved on the hub and invisible on screen; the same reload makes a teammate's edit appear in a page you are already looking at. EditView's banner is raised by the MERGE VERDICT for the source editor and by the change stream only for the visual one: the verdict knows whether the write could be folded in and the event cannot, so raising a banner on the event would flash the wrong answer ahead of it. For that visual path `mine` is a COUNT, not a flag: the change stream announces a write as soon as the hub journals it, often before the PUT's own response, so openSharedFile reports a write BEFORE it goes out, and two saves in flight (routine — clicking between paragraphs saves each) left the second event with nothing to claim it and raised the peer banner on the user's own edit"
    note for lib "A save carries the version its buffer was read at (If-Match), and a 409 is not an error to retry — it means somebody else's write is already the file. By then merge() has necessarily declined, because a save only happens when this buffer has changes of its own, so there is nothing left to reconcile: the losing version is written BESIDE the file as <name>.bdrive-conflict-<who>-<utc>, the same name the sync path has used since the beginning and the same one ConflictBanner already explains. conflictName is therefore a second implementation of a pure function that lives in Go (syncer.go) — round-tripped through parseConflict in the unit tests, because two formats for one filename would be two explanations for one reader."
    note for lib "collab.ts publishes a caret at most every 200ms, only its OWN client state, and not at all when nobody else is in the room. Every awareness change used to be its own POST, so arrow-keying around a file spent a request per keypress drawing a caret for nobody. Staying quiet while alone is only safe with the two forced announcements around it — the first one, and an answer to each new arrival — because awareness is relayed and never logged, so an announcement is lost to anyone who shows up after it and two people would otherwise stay invisible to each other forever."
    note for lib "sharedfile.merge is how an agent's write reaches a document somebody is typing in. The editor still never re-seeds itself from the server — that resets the buffer under a cursor — so the write arrives as textEdit's single splice instead: everything it does not change is untouched, and so is the caret sitting in it. It REFUSES in the two cases where a splice destroys something, and the refusal is FileView's banner: unsaved local edits (never overwrite half a sentence), and a co-editor in the room (both clients would compute the same splice, and two identical splices into one CRDT is the change applied twice). `saved` moves to the incoming text BEFORE the splice, or the document change it causes schedules a save that writes back what was just read. A relay-less surface has no Y.Text to splice, so it passes soloApply — without one merge reports blocked rather than claiming a change it could not make"
    note for lib "sharedfile.ts is everything about having a file open that is not about a keyboard — join the room, hold the CRDT, save on idle, save once more on the way out. Both surfaces sit on it: Editor binds CodeMirror to the Y.Text, VisualEdit splices ranges into the same one, so somebody typing markup and somebody clicking a headline are in one room and neither has to know the other exists"
    note for components "components/ui — shadcn/ui primitives (Radix, copied in), themed from BearDrive tokens in tw.css; rendered markdown is transformed as a string before mounting, link clicks delegated on the container — never patch the dangerouslySetInnerHTML subtree"

    class lib {
        +diff.ts splitLines lcsDiff diffText
        +diff.ts textEdit (the one splice that folds an outside write in)
        +runs.ts groupRuns runFileCount
        +heat.ts heatFor heatTotal heatText heatLevel hotPathSplit
        +heat.ts HEAT_DISCLOSURE (what the count includes, said once)
        +heat.ts ageRange isFlatRange ageSpanLabel (treemap scale)
        +heat.ts orphanPaths (reads whose file left the tree)
        +heat.ts placeLabels LABEL_MAX (scatter danger-dot labels)
        +heat.ts HOT_READS STALE_DAYS isDanger daysSince agoLabel staleNote
        +conflict.ts parseConflict conflictName Conflict
        +collab.ts CollabDoc peerCount (Yjs over SSE + POST)
        +sharedfile.ts openSharedFile SharedFile SAVE_IDLE_MS
        +sharedfile.ts merge MergeResult (an outside write, offered to an open document)
        +sharedfile.ts preserve (a save that lost, parked beside the file)
        +sniff.ts sniffBytes BlobText MAX_BYTES
        +scroll.ts Goal armGoal applyGoal noteScroll MAX_APPLY
        +csv.ts parseDelimited Csv CSV_ROWS
        +fuzzy.ts fuzzy fuzzyStemmed scoreLabel
        +prompt.ts INSTALL_DOC agentPrompt
        +secrets.ts SecretFinding secretsMessage secretsBadge
        +mermaid.ts hasMermaid renderMermaid Palette DARK LIGHT
        +utils.ts
    }
    note for lib "mermaid.ts is the one exception to 'pure, no React, unit-tested on node': it needs a DOM and a browser-only library, so its coverage is Playwright. html in → html out, so neither caller can be tempted to patch a live subtree. It imports mermaid only when hasMermaid() says a document has a fence — that gate is what keeps a diagram-free page from downloading any of it — and every failure (unparseable fence, render throw, chunk that never loads) returns the untouched &lt;pre&gt;&lt;code&gt; instead of throwing"
    note for lib "pure, no React, unit-tested on node (npm test) — the line diff is ~40 lines, cheaper than auditing a diff package. heat.ts is the one read-count arithmetic: every surface (file header, folder listing, Dashboard bar) totals and splits through it, so they cannot disagree; useBrowse re-exports it. HEAT_DISCLOSURE sits beside that arithmetic for the same reason: a member's own views count toward the number, and four surfaces printing their own copy of that promise is four promises that can drift (BEA-61). The constant is NOT re-exported through useBrowse — surfaces import it straight from lib/heat, and a unit test asserts src/ holds exactly one copy of the sentence. The hot-and-stale VERDICT joined the totals for the same reason (BEA-119): HOT_READS/STALE_DAYS/isDanger were private to Insights.tsx, so the Dashboard was the only screen that could say a doc was hot and unmaintained — the file page and the folder listing showed the ingredients and no verdict. isDanger takes (reads, days) rather than a heat entry because only the Dashboard has a reader lens: it passes its lens-filtered count, the other two pass heatTotal. staleNote returns a STRING (empty when not flagged) so the badge stays pure and survives whichever component owns the meta line"
    note for lib "scroll.ts is the per-route scroll goal, lifted out of Browser.tsx so it can be tested at all — the frontend suite is node --test over pure TS with no jsdom. A route change arms a goal (0 on a fresh navigation, the memoized offset on POP) and views re-apply it up to MAX_APPLY times as content grows after first paint; noteScroll RETIRES it the moment the reader scrolls, which is the choke point that stops any onRendered caller — a metadata refresh, a poll, anything added later — from moving a reader mid-read (BEA-155). Two clauses in that guard are load-bearing and each has its own test: scrollTo fires a scroll event of its own, so movement is measured against the goal and never against zero, and a page shorter than the last one makes the browser CLAMP the carried-over offset to the bottom, which is either the old page arriving or a goal that does not fit yet"
    note for lib "secrets.ts is the six credential rules in words, mirroring internal/secrets' Label map. It lived in Browser.tsx under a comment saying one caller did not justify a file; BEA-147 gave it a second one, and the two callers are the two surfaces that report the SAME finding — the share dialog that refuses to mint, and FileView's badge that only warns. One map is what makes 'wording consistent with the share dialog' mechanical rather than a copy-editing promise"
    note for lib "conflict.ts recognises a conflict copy from its NAME alone — syncer.conflictName is a pure function of the path, so the device and the moment come out of the string with no server route, no journal field and no request. The regex is an ANCHORED suffix and a strictly narrower match of the Go convention (sanitize's character class, clip's 32), and every mismatch — truncated suffix, impossible date — is null rather than a throw, so a stray filename can never break a listing. Two callers: FolderListing marks the row, ConflictBanner explains the file (BEA-128)"
    note for lib "fuzzy.ts is the palette matcher, lifted out of Palette.tsx so it can be tested at all. scoreLabel is the whole query: it splits on whitespace and every token must match the label (AND, order irrelevant, scores summed) — a space used to be just another character to the subsequence walk, and a path never contains one, so any two-word query matched NOTHING (BEA-194). Its typo pass is one rule, not an edit-distance routine: retry a token of 4+ characters with each single character dropped, which covers a transposition, a substitution and an insertion because subsequence matching already tolerates the other direction. Three properties are load-bearing and each has a test: a blank query returns score 0 with no hits BEFORE it splits (the stable sort over that is what keeps the project's own destinations on top — BEA-52, BEA-105), an errored token costs a flat penalty no bonus can climb over (so exact beats approximate by construction, not by tuning), and hits come back sorted and de-duplicated — Highlight slices forward from the last index and would silently duplicate letters otherwise. Palette runs it strict over every candidate and retries only the misses, only when the strict pass came back short of the 40-row cap, so a query that already matches never pays for the typo pass"
    note for lib "prompt.ts is the agent paste-prompt — the single most important string in onboarding, and it was assembled by hand in ConnectGuide, EmptyState and Setup's Done frame, three wordings drifting independently. They differ only in what is being set up, so they are one function with one varying clause. Pure and origin-injected so the node suite can test it: each previous wording is pinned by a test, and one more pins that every variant still ASKS which folder, which is the runbook's hard gate"
    note for lib "csv.ts parses .csv/.tsv for FileView's table view — ~50 lines against RFC 4180, so no papaparse. It NEVER throws: null means 'not a table' (unterminated quote, no delimiter) and the caller falls back to the plain-text preview, which is why the fallback is a type-level guarantee rather than a try/catch someone can forget"

    ErrorBoundary --> App : wraps the whole tree
    App --> HubApp
    App --> VolumeApp
    HubApp --> Browser
    VolumeApp --> Browser
    HubApp --> router
    Browser --> router
    Browser --> lib : parseConflict
    Browser --> components
    components --> lib : openSharedFile (Editor and VisualEdit share one room)
    HubApp --> components
    components --> nav : linkProps navigate
    components --> lib : diffText groupRuns hotPathSplit placeLabels staleNote isDanger parseDelimited renderMermaid parseConflict secretsBadge scoreLabel
    Browser --> lib : secretsMessage (the share dialog's half of lib/secrets)
    Browser --> lib : armGoal applyGoal noteScroll
    hooks --> lib : re-exports heat.ts, sniffBytes
    shareMermaid --> lib : renderMermaid
    hooks --> api
    Browser --> hooks : useTree useHeat useShares useProjectEvents, fetchBlobText + fileURLFor (Copy)
    HubApp --> hooks
    HubApp --> Setup : desktop mode only, /setup/* frames
    HubApp --> McpConnections : /connections, account-level
    Setup --> api : /api/desktop/init/{start,status}
    Setup --> nav : each frame owns a URL
    hooks --> analytics : initAnalytics + identify on config
    api --> analytics : track(product event)
    Browser --> analytics : share_created (the one raw fetch)
```
