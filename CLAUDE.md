# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

**BearDrive** is the product name; **`bdrive`** is its CLI binary (file conventions use the full name: `.beardrive`, `.beardriveignore`, `~/.beardrive`, `BEARDRIVE_HOME`). BearDrive is a Go CLI that mounts any folder as a synced volume: contents sync across devices through cloud object storage (S3, GCS, S3-compatible, or a plain directory), with per-file change history and offline support. No server — devices converge through append-only journals in a dumb object store.

The repo ships one binary: `cmd/bdrive` — the CLI, the sync daemon, and the read-only web viewer (`bdrive web`).

## Commands

```sh
go build ./...                                   # build everything
go test ./...                                    # run all tests
go test ./internal/syncer -run TestConflict -v   # run a single test
go vet ./...                                     # vet
go build -o bdrive ./cmd/bdrive                  # build the binary (gitignored at repo root)
```

There is no Makefile, linter config, or CI config in-repo. Releases run `goreleaser release` on a tagged commit (see `.goreleaser.yaml`); the version is injected via `-ldflags "-X main.version=..."` into `cmd/bdrive/main.go`.

When testing the CLI manually, set `BEARDRIVE_HOME=/some/tmp/dir` to relocate all beardrive state (device identity, mount registry, volume stores) away from the real `~/.beardrive`.

## Architecture

Data flows in two hops; the local volume store is the pivot:

```
working folder  ←scan/materialize→  volume store (~/.beardrive/volumes/<vol>)  ←push/pull→  object store
 (real files)                       blobs/ + journal/ + state + sync                  s3:// gs:// file://
```

Package roles (`internal/`):

- **`journal`** — the core data model. Every change is an `Op` (`put`/`delete`) in a per-device append-only JSONL log. `Less` defines the total order `(lamport, time, device, seq)`; `Replay` folds all ops into the volume state, last-writer-wins per path. Everything else is machinery around this.
- **`store`** — a volume's local on-disk state: content-addressed blob store (`blobs/<aa>/<sha256>`), per-device journal copies, the per-mount materialization cache (`state-<mountID>.json`, size+mtime fingerprints for cheap change detection), sync state (lamport clock + push cursor), and the exclusive flock that serializes cycles.
- **`remote`** — the `Backend` interface (Put/Get/List/Exists) with `file://`, `s3://`, `gs://` implementations. Remote layout: `blobs/<sha256>` + `journal/<device>.jsonl` under the URL prefix.
- **`syncer`** — the heart: `Session.Cycle()` runs one pass: scan → commit local ops → pull peer journals → preserve conflict copies → materialize merged state → push blobs + own journal. Read the package doc comment in `syncer.go` first. `ignore.go` holds the path filter (`.beardriveignore` rules + the `.beardrive` include list), applied symmetrically in scan and materialize; a newly filtered path is dropped from the cache *without* a delete op so opting out locally never deletes remotely.
- **`daemon`** — per-mount background loop (detached process, pidfile `daemon-<mountID>.pid` and log `daemon-<mountID>.log` in the volume dir). Scans every `--scan-interval` (3s), talks to the remote every `--remote-interval` (10s) or immediately after local edits. Re-reads `mounts.json` each tick to pick up `bdrive remote set` / `umnt --forget` without restart.
- **`config`** — global state under `$BEARDRIVE_HOME` (default `~/.beardrive`): device identity (`device.json`), mount registry (`mounts.json`), `MountID()` (sha256 of the folder path — one volume can be mounted at several folders, and everything folder-specific is keyed by it). Also the per-folder `.beardrive` project file (`project.go`): volume/remote/include settings that live in the mounted folder itself, win over the registry (`EffectiveMount`), and are never synced.
- **`webapp`** — the `bdrive web` server: a `Source` interface with two implementations — `DirSource` (serves a local folder straight from disk; the default when no remote is given) and `RemoteSource` (reads journals straight from the remote, no local store, folds them into a file tree with per-file provenance). Renders markdown (goldmark + Obsidian `[[wikilinks]]`), streams/downloads content. Frontend is dependency-free vanilla JS embedded via `go:embed static`.

`cmd/bdrive/` is a thin cobra CLI over these packages (`mnt`, `umnt`, `sync`, `status`, `log`, `remote`, `web`, `whoami`, `daemon`, `version`).

## Invariants — do not break these

- **Each device writes only its own journal.** This is the whole concurrency story: no locking service is needed because no object ever has two writers. Never write to another device's journal file or remote key.
- **Blobs are pushed before the journal** (`syncer.push`), so a peer never sees an op whose content is missing. Preserve this ordering.
- **Scan happens before pull** in `Cycle`, so local edits are journaled (and content captured) before remote state can overwrite the working folder.
- **Replay must stay deterministic.** Any change to `journal.Less` or `Replay` changes what every device converges to.
- **Materialize never clobbers dirty files**: a file whose size/mtime differs from the state cache changed mid-cycle and is left for the next scan.
- **All state files are written atomically** (temp file + rename, see `store.WriteFileAtomic`). Temp files are prefixed `.beardrive-tmp-` and ignored by the scanner.
- **`Cycle` runs under the volume flock** — the daemon and one-shot CLI commands (`bdrive sync`) coexist through it.
- Errors during pull/push degrade to `Result.Offline` rather than failing the cycle; unreadable/vanished files during scan are skipped and retried next cycle. Follow this "never break sync, retry next cycle" posture.

## Testing conventions

The real coverage is the integration tests in `internal/syncer/syncer_test.go`: each test builds multiple simulated devices (`newDevice`) syncing through a shared `file://` remote (`sharedRemote`), then drives explicit `cycle()` calls to test convergence, offline operation, and concurrent-edit conflicts. Extend these when touching sync behavior — a new sync feature without a multi-device test is untested where it matters.

## Claude Code plugin

`plugin/` is a Claude Code plugin (skill + `/beardrive:mount` + `/beardrive:status` commands + turn-boundary sync hooks), published via the marketplace manifest at `.claude-plugin/marketplace.json` (`/plugin marketplace add runbear-io/beardrive`). The canonical skill lives at `plugin/skills/beardrive/SKILL.md`; `.claude/skills/beardrive` is a symlink to it. The hook script `plugin/scripts/beardrive-sync.sh` must stay a fast no-op for folders without a `.beardrive` file — it runs on every turn in every project.

## Docs to keep in sync

- `README.md` and `plugin/skills/beardrive/SKILL.md` both document CLI behavior, flags, output formats, and the on-disk layout. When changing CLI commands, flags, output, or layout, update both — the skill is what makes Claude Code beardrive-aware for end users and must match the actual binary.
