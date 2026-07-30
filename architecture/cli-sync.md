# `bdrive` CLI & sync engine — class diagram

Source of truth: `cmd/bdrive` (commands, gates) and `internal/{syncer,store,
journal,config,daemon,agenthooks,autostart}`; the `internal/remote` seam is drawn in
[webapp-server.md](webapp-server.md). Reflects the code as of this commit;
update this file in any PR that changes these types or their relationships.

## Sync engine — one cycle

```mermaid
classDiagram
    direction LR

    class Session {
        +Folder string
        +MountID string
        +Store *store.Store
        +Device config.Device
        +Account config.Settings
        +Backend remote.Backend
        +Note string
        +Prune bool
        +OnProgress func
        +Cycle(ctx) Result
        +Restore(ctx, path, sha) error
    }
    note for Session "Restore writes a historical blob back into the working folder as an ordinary edit (fetching it from the hub when this device never held it) — the next Cycle journals it like any other change; it takes no lock and appends to no journal itself"
    note for Session "internal/syncer — scan → commit local ops → pull peer journals → preserve conflicts → refresh rules → prune → materialize → push blobs then own journal"
    note for Session "Prune (bdrive forget / sync --prune, never the daemon) journals a delete for every replayed path the SHARED ignore rules exclude — the include scope is per-device and must never prune it"

    class Result {
        +LocalOps +PulledOps
        +Conflicts +Pruned +Materialized
        +Pushed +Offline +OfflineErr
        +ReadOnly +NoAccess +AccessErr
    }
    note for Result "Offline / ReadOnly / NoAccess are three different answers: unreachable (retry all), push refused (pull-only), pull refused (pause, touch nothing)"

    class Filter {
        +Skip(rel) bool
        +PruneDir(rel) bool
    }
    note for Filter "ignore.go — .bdriveignore rules (incl. the managed `# bdrive scope` negation block written by init --only / bdrive scope) + a legacy .bdrive include list, applied symmetrically in scan and materialize; Negated() is what makes sync --prune refuse on a scoped project. NOT the whole predicate: walkFolder adds .git/.bdrive pruning, non-regular and .DS_Store/.bdrive-tmp-* skips, and nested-mount handoff"

    class walkFolder {
        +walkFolder(folder, filter, fn)
        verdict: vSync vSkipFile vDescend vPruneDir vNested
    }
    note for walkFolder "walk.go — the ONLY copy of the sync predicate; scan and Explain both go through it, so what --explain reports cannot drift from what leaves"

    class Explain {
        +Explain(folder, include) two lists
        +NotSyncedFiles(entries) int
    }
    class Entry {
        +Path string
        +Files int
        +Nested bool
    }
    note for Explain "explain.go — bdrive scope --explain. Pure read: own Filter, no Session, no flock, no network. Collapses fully-excluded dirs to one counted line; nested mounts annotated, counted as zero (they sync via their own project)"

    class Store {
        -dir volume dir
        +PutBlob / OpenBlob / HasBlob
        +AppendOps / DeviceOps / AllOps
        +LoadCache / SaveCache mountID
        +LoadSync / SaveSync
        +SaveNote / LoadNote
        +PendingReads read spool
        +Lock() flock
    }
    note for Store "internal/store — ~/.bdrive/volumes/mount-id: content-addressed blobs, per-device journal copies, state cache, paused marker (free funcs Paused/SetPaused, no flock)"

    class Op {
        +Seq +Lamport +Time +Device
        +Author +User +UserName
        +Kind put or delete
        +Path +Blob +Size +Mode +Note
    }
    note for Op "internal/journal — Less orders by (lamport, time, device, seq); Replay folds to LWW-per-path state; each device writes only its own journal"

    class Backend {
        <<interface>>
        +Put +Get +List +Exists +Close
        +ErrForbidden sentinel
    }
    note for Backend "internal/remote — client devices use the https:// hub backend (token from BDRIVE_TOKEN / settings.json); a hub 403 wraps ErrForbidden, which is what Result turns into ReadOnly/NoAccess instead of Offline"

    class daemon {
        +Run(folder, scan, remote)
        +Start / Stop / Running
    }
    note for daemon "per-mount detached loop; re-reads .bdrive/config.json each tick, exits without deletes if it vanishes"

    Session --> Store : volume state
    Session --> Backend : pull and push
    Session --> Filter : scan and materialize
    Session --> walkFolder : scan
    Explain --> walkFolder : same predicate
    Explain --> Filter : own fresh instance
    Explain ..> Entry : not-synced lines
    walkFolder --> Filter : Skip / PruneDir / addNestedMount
    Session ..> Op : commits, replays
    Session --> Result
    Store o-- Op : journal files
    daemon --> Session : one Cycle per tick
```

## CLI commands, device state, and the opt-in gate

```mermaid
classDiagram
    direction LR

    class Commands {
        init login logout
        sync stop scope forget status log
        restore url share export import
        web daemon hooks read-log
        resume autostart
    }
    note for Commands "cmd/bdrive — thin cobra layer; init is the front door (one command: login + hooks + sync + link), stop pauses"

    class syncBlocked {
        <<gate>>
        enrolled in mounts.json?
        volume not paused?
    }
    note for syncBlocked "cmd/bdrive/helpers.go — sync, sync --hook, and read-log must pass it; reads the registry WITHOUT ResolveMount's enrolling self-heal. Hook mode fails silent; plain sync errors with a bdrive init pointer"

    class openSession {
        mustProject → ResolveMount
        store.Open + remote.Open
    }
    class startSync {
        enroll + clear paused
        initial Cycle
        daemon.Start
    }
    note for startSync "cmd/bdrive/sync_run.go — init's engine; the ONLY enroller and the only thing that resumes a pause"

    class Project {
        +ID stable mount id
        +Volume +Remote
        +Include legacy, read-only
    }
    note for Project ".bdrive/config.json — travels with the folder (git clone, copy); presence alone is NOT consent to sync"

    class MountRegistry {
        mounts.json
        id → Path Volume Remote
    }
    class Device {
        device.json
    }
    class Settings {
        settings.json
        server + token + account
    }
    note for MountRegistry "internal/config — per-device state under BDRIVE_HOME; ResolveMount self-heals the path for enrolled mounts (renames/moves stay free)"

    class AgentHooks {
        Detect / Install / Uninstall / Registered
        ConfigPath = USER config
        turn-start: sync --hook
        post-edit: sync --note
        post-read: read-log
    }
    note for AgentHooks "internal/agenthooks — registers per-platform hook commands (claude, codex, gemini, hermes) in each platform's USER config, once per machine; they fire in every folder, every turn, and no-op outside mounts"

    class PausedMarker {
        volumes/id/paused
    }
    note for PausedMarker "set by bdrive stop, cleared only by bdrive init (startSync)"

    class Autostart {
        Install / Uninstall / Installed / Path
        ErrUnsupported (non-darwin)
    }
    note for Autostart "internal/autostart — ONE login unit per machine (darwin: ~/Library/LaunchAgents plist, RunAtLoad, no KeepAlive) that runs `bdrive resume`; writes the file only, never shells out to launchctl"

    class DaemonLock {
        volumes/id/daemon.lock
        volumes/id/daemon.pid
    }
    note for DaemonLock "internal/daemon — liveness is the flock, held for the daemon's lifetime; the kernel drops it at death/reboot, so a leftover pid can never read as running (pid is display + signal only)"

    Commands --> Autostart : autostart install/uninstall (init runs install automatically)
    Autostart ..> Commands : login runs `bdrive resume`
    Commands --> DaemonLock : Running / Start / Stop
    Commands --> AgentHooks : hooks install/uninstall (init runs install automatically)
    AgentHooks --> Commands : runs sync and read-log
    Commands --> syncBlocked : sync and read-log gate first
    syncBlocked --> MountRegistry : reads only, never enrolls
    syncBlocked --> PausedMarker : Paused check
    Commands --> openSession : after the gate
    openSession --> MountRegistry : path self-heal (enrolled only)
    Commands --> startSync : init
    startSync --> MountRegistry : enrolls
    startSync --> PausedMarker : clears
    Commands --> PausedMarker : stop sets
    openSession ..> Project : loads
    openSession ..> Device : identity
    openSession ..> Settings : account and token
```
