# Delta sync — shared goal

Read this file first, every round. It is the only definition of "done".
Nothing else — not a passing build, not a benchmark screenshot, not a
paragraph explaining that chunking is implemented — ends the loop.

Design and rationale: [`docs/delta-sync-prd.md`](../docs/delta-sync-prd.md).
This file is the contract; the PRD is the reasoning behind it. Where they
disagree, this file wins and the PRD gets fixed.

## Mission

A changed file must transfer only its changed regions, on push and on pull,
**without changing a single byte of the journal format** and without altering
what any device converges to.

## The one rule

**A saving does not exist until a byte-counting test asserts it, and no saving
is accepted unless a convergence test lands in the same commit.**

Bytes are the whole point of this work, so "we implemented chunking" is not a
result — a number is. And a transfer that shrinks while convergence drifts is
not an optimization, it is data loss with a faster wire.

Symmetrically: an invariant is not protected because someone was careful. It is
protected by a test that fails when it breaks. Tests are never deleted,
skipped, or loosened to close a row.

## What counts as done

The loop ends when **all three** hold:

1. Every scoreboard row is `pass`, each backed by a named Go test in the repo.
2. `go build ./... && go vet ./... && go test ./...` is green on a clean tree.
3. Every row in the **E block** passes — including the two that require a
   binary built from the merge-base commit. Nothing ships to users on a green
   in-process suite alone.

There is no "mostly there" and no percentage. A row passes or it is open.

## Hard stop: the measurement gate

**No production code ships past the counting harness until the go/no-go row has
a number in it.**

BearDrive syncs markdown and code. For a 4 KB file, chunking costs more than it
saves. If the corpus turns out to be small text files, the correct outcome of
this entire loop is: ship transport compression (there is none today — no gzip,
no zstd, no `Content-Encoding` anywhere in `internal/remote`,
`internal/syncer`, or the hub proxy), record the number, and stop.

Shipping chunking against a corpus that does not need it is a failure of this
goal, not a partial success.

## The counting harness

Everything below is measured through one ~30-line test decorator over
`remote.Backend`, wrapping the `file://` store that `sharedRemote`
(`internal/syncer/syncer_test.go:75`) already returns and dropping into
`newDevice` (`:61`) unchanged:

```go
// countingBackend records bytes crossing the wire in both directions.
type countingBackend struct {
    remote.Backend
    put, get atomic.Int64
}
```

Build this first. It is the instrument every acceptance criterion reads.

## Test conventions

- Named `TestDelta_<Area>_<Behavior>` (e.g. `TestDelta_Push_FrontInsertion`);
  E-block rows are `TestDeltaE2E_<Behavior>`.
- Sync behavior lives in `internal/syncer/delta_test.go`, built on the existing
  multi-device pattern (`newDevice` + `sharedRemote` + explicit `cycle()`
  calls). A sync change without a multi-device test is untested where it
  matters.
- Hub behavior lives in `internal/webapp/chunks_test.go`.
- End-to-end rows live in `internal/webapp/delta_e2e_test.go` and **must** use
  `newCLIEnv` (`cli_e2e_test.go:41`) — it `go build`s the real binary, runs it
  against a real `startTestHub` over real HTTP with an isolated `HOME`.
- Export/import lives with `cmd/bdrive/migrate.go`'s existing tests.
- `sandbox/` is not used. All of this is deterministic and machine-local, so
  all of it is Go tests.

**In-process tests do not close an E row.** A row in the E block is closed only
by a test that drives the real binary over real HTTP. Anywhere else,
in-process is fine and preferred — it is faster and the coverage is the same.

No Playwright. The browser never touches blob content directly; every read
funnels through hub handlers that E2/E3 already cover with real bytes over real
HTTP, so a browser driver would re-test the same choke point through a slower
harness. (Also checked: the hub serves no `Range` requests today — content is
`io.Copy`'d — so reassembly cannot regress partial-content behavior that does
not exist.)

## Scoreboard

Fill in the test name when a row passes. A row with no test name is open,
whatever the state column says.

### Gate — measurement

| # | Criterion | Owner | Test | State |
|---|---|---|---|---|
| G1 | The counting backend counts Put/Get bytes accurately | agent | `TestDelta_Harness_CountsBytes` | **pass** |
| G2 | Baseline recorded: 1-byte edit to a 20 MB file pushes ~20 MB today | agent | `TestDelta_Baseline_WholeFileCost` | **pass** |
| G3 | Size + churn distribution measured on ≥3 real projects | **human** | — (numbers in PRD §Phase 0) | **pass** |
| G4 | Transport compression measured standalone | agent | `TestDelta_Gzip_TextCorpusRatio` | **pass** |
| G5 | **Go / no-go recorded, with the number that decided it** | **human** | — | **pass** |

> G2 is a characterization test. When delta lands it is **updated** to the new
> bound, never deleted — it is the before-and-after in one place.
>
> **G3 and G5 cannot be closed by an agent and must never be guessed.** G3
> needs real user corpora, which are not in this repo; G5 is a product
> decision. An agent that reaches them stops, reports what G1/G2/G4 measured,
> and asks. Inventing a plausible-looking distribution to get past the gate is
> the single worst thing that can happen in this loop — it converts a
> measurement into a rationalization and every later round inherits it.

### Transport — the savings

Bound is "small multiple of the max chunk size", not "less than the file".

| # | Criterion | Test | State |
|---|---|---|---|
| D1 | 1-byte edit mid-file, 20 MB file: push < 5 MiB | `TestDelta_Baseline_WholeFileCost` | **pass** |
| D2 | Peer holding the basis pulls that edit in < 5 MiB | `TestDelta_Pull_SmallEdit` | **pass** |
| D3 | Append to a 20 MB file: push < 5 MiB | `TestDelta_Push_Append` | **pass** |
| D4 | **Insertion at the START of a 20 MB file: push < 5 MiB** | `TestDelta_Push_FrontInsertion` | **pass** |
| D5 | Files ≤ 4 MiB write no `chunks/` key at all | `TestDelta_Threshold_SmallFileUnchanged` | **pass** |
| D6 | Cold pull with no basis is one whole-blob GET | `TestDelta_Pull_ColdPathUnchanged` | **pass** |

> D4 is the one that matters. Fixed-size blocking passes D1 and D3 and fails
> D4 — it is the proof that boundaries are content-defined and not an offset
> table. If D4 is red, the chunker is wrong no matter what the others say.

### Correctness — non-negotiable

| # | Criterion | Test | State |
|---|---|---|---|
| C1 | **Journal bytes for a given edit are identical to the pre-change binary** | `TestDelta_Journal_ByteIdentical` | **pass** (in-process half; E2/E3 complete it) |
| C2 | 3 devices, offline edits on two, conflict copies identical to whole-blob behavior | `TestDelta_Converge_ThreeDeviceConflict` | **pass** |
| C3 | Assembled content is verified against `Op.Blob` before entering the blob store | `TestDelta_Pull_RejectsMismatch` | **pass** |
| C4 | A manifest that does not reassemble to its key is refused; nothing is backfilled | `TestDelta_Manifest_SelfVerifying` | **pass** |
| C5 | Interrupt during chunk upload: peer sees no broken op, next cycle heals (manifest-stage refusal now falls back to whole-blob — H4) | `TestDelta_Order_ChunksBeforeManifest` | **pass** |
| C6 | Interrupt between manifest and journal: same | `TestDelta_Order_ManifestBeforeJournal` | **pass** |
| C7 | One missing chunk does not abandon the batch — every complete op behind it still lands | `TestDelta_Pull_MissingChunkDoesNotStall` | **pass** |
| C8 | Chunked-only project survives `export` → `import` with full fidelity | `TestDelta_Migrate_RoundTrip` + `TestDelta_Import_RejectsCorruptChunk` | **pass** |

> C1 is the strongest guard in this table. It is what makes "no journal format
> change" a fact instead of an intention, and it is what keeps `journal.Less`
> and `Replay` — and therefore every device's convergence — out of scope.
>
> C7 mirrors a bug this repo already fixed once on the blob path
> (`syncer.go:936`): one unfetchable object must not stop everything queued
> behind it. Chunks inherit the same rule and the same test shape.

### Hub

| # | Criterion | Test | State |
|---|---|---|---|
| H1 | Reassembly backfills `blobs/<sha>` once; the second read does not re-reassemble | `TestDelta_Hub_BackfillOnce` | **pass** |
| H2 | Chunk presigning refuses a key that already exists (sealing invariant) | `TestDelta_Hub_ChunkPresignRefusesExisting` | **pass** |
| H3 | Manifests are never presigned — always server-relayed | `TestDelta_Hub_ManifestNeverPresigned` | **pass** |
| H4 | `validStoreKey`, the list prefix allowlist, and the put hash check accept and constrain both new key classes | `TestDelta_Hub_KeySpace` | **pass** |

### E — end to end, real binary only

These close **only** with `newCLIEnv`. An in-process reproduction of one of
these rows is a useful test and does not close the row.

| # | Criterion | Test | State |
|---|---|---|---|
| E1 | Two real `bdrive` processes, real hub: a 20 MB file edited on one converges on the other, and the wire cost is < 5 MiB | `TestDeltaE2E_TwoDevicesLargeFile` | **pass** |
| E2 | **A binary built from the merge-base commit syncs a project whose storage holds only chunks and manifests** — full pull, correct bytes on disk | `TestDeltaE2E_OldBinaryReadsChunkedStorage` | **pass** |
| E3 | **A binary built from the merge-base commit pushes; a current binary pulls** — converges byte-identically | `TestDeltaE2E_OldBinaryWritesNewReads` | **pass** |
| E4 | Real `bdrive export` → real `bdrive import` → third real device syncs the imported project and gets correct bytes | `TestDeltaE2E_MigrateRoundTrip` | **pass** |
| E5 | Viewer, history `/blob`, share link, and download serve correct bytes for a chunked-only file over real HTTP | `TestDeltaE2E_AllReadSurfaces` | **pass** |
| E6 | Daemon path, not just one-shot `sync`: a chunked edit propagates through a running daemon | `TestDeltaE2E_DaemonPropagates` | **pass** |

> **E2 and E3 are the reason this block exists.** Every other row in this file
> can be satisfied by code that was written knowing about chunks. These two
> cannot: they require a binary that has never heard of a manifest, and the
> only honest way to get one is to build it —
> `git worktree add <tmp> $(git merge-base HEAD main)` then `go build` in
> there. Simulating an old client by disabling a code path in the current tree
> tests the flag, not the compatibility. If these are red, upgrading a hub
> breaks every device that has not upgraded yet, which is the single worst
> outcome this project can produce.
>
> E6 exists because every other sync row drives `cycle()` or `bdrive sync`
> directly. The daemon is how sync actually runs for real users, and it has its
> own lifecycle (flock, intervals, config re-read) that one-shot calls skip.

## Invariants that fail the round outright

Break any of these and the round does not close, regardless of the scoreboard:

1. **Each device writes only its own journal.** Chunks and manifests are
   content-addressed with no per-device keys. Keep it that way.
2. **Content before journal**: `chunks → manifest → journal`, never reordered.
3. **No `Op` field is added, removed, or read differently.** `journal.Less` and
   `Replay` are not touched. (C1 enforces this.)
4. **Scan before pull** in `Cycle`.
5. **Materialize never clobbers dirty files.** Assembly happens in the volume
   store, never in the working folder.
6. **Never break sync, retry next cycle.** A missing chunk, a bad manifest, or
   a failed reassembly degrades to `Result.Offline` or skips that path. Record
   the first error, finish the batch, return it once — the posture `pull`
   already has.
7. **No read returns bytes that do not hash to the sha requested**, hub-side or
   client-side.
8. **The local volume store layout does not change.** `store.OpenBlob`,
   `PutBlobFile`, `writeFile`, `materializeFile` stay as they are.

## Known trap

The store layout is enumerated in more places than it looks, and one of them
loses data in silence. **All of these land before anything writes a chunk:**

`cmd/bdrive/migrate.go:193` (export), `:280` (import), `:35` (`blobKeyRe`, a
duplicate of the hub's), `webapp/store.go:34` (`validStoreKey`), `:176` (list
prefix allowlist), `:528` (put hash check).

Miss the first one and `bdrive export` produces an archive that is missing file
content, with no error — the anti-lock-in story quietly broken.

## Round protocol

Each round:

1. Re-read this file.
2. Pick the topmost open row. Order is Gate → Hub key space → Transport →
   Correctness → E. Correctness rows for a phase land with that phase, never
   after. **E2 and E3 are the exception to "topmost": open them as soon as
   anything writes a chunk**, because they are the rows most likely to force a
   design change, and the cost of discovering that last is the whole
   implementation.
3. Write the test. Run it. Paste the failure.
4. Implement until it passes and `go test ./...` is green.
5. Update the row with the test name and `pass`.
6. If a round produces no passing row, say so plainly and say what blocked it.
   A round that ends with a summary and no scoreboard change is a failed round.

Anything you suspect but cannot reproduce goes in a `## Leads` section at the
bottom of this file. A lead is not a finding and never closes a row.

### When a row cannot be closed as written

Two escapes, and no third:

- **The row is human-owned** (G3, G5) or needs something outside this repo:
  stop the loop, report what is measured so far, and ask. Do not estimate, do
  not proceed to the next row, do not mark it `pass` with a caveat.
- **The design is wrong.** If a row — most likely E2 or E3 — cannot be made to
  pass without violating an invariant, that is a finding about the design, not
  a reason to weaken the row. Say so, propose the change, and update
  `docs/delta-sync-prd.md` in the same commit as the fix. The PRD is expected
  to change; this file's rows and invariants are not.

Working around a row is never one of the escapes. A row loosened to make a
round end has cost more than it saved.

## Leads

- ~~Manifest write-once pins the chunker parameters~~ **Downgraded (CTO round
  4): the params are no longer load-bearing for correctness anywhere.** A
  manifest refusal (write-once 409 or ingest 400) falls back to a whole-blob
  push, so a future change to `chunkPol`/`chunkMin`/`chunkMax` costs one full
  upload per affected file instead of a wedge or a migration. Change them
  freely if a better set is found; dedup across the boundary degrades, sync
  does not.
- **Reassembly backfill still bypasses quota accounting** (CTO M2): the
  hub-authored `blobs/<sha>` write records no usage. Harmless under OSS
  UnlimitedQuota; the managed layer should wire RecordUsage at that seam.
- **Chunk transfer is serialized per file** (CTO M1): a 100 MiB cold push
  over HTTP is ~100 sequential round trips. A bounded errgroup inside
  pushChunked/fetchChunked when it shows up in real use.
- **No concurrency bound on hub reassembly spools** (CTO M3): 256 MiB × N
  concurrent legacy reads; `/s/` is per-IP rate-limited only. A small
  semaphore if it shows up.
- **Transient content-fetch failures are never retried until the path's
  journal grows.** Pull's fetch loop covers only newOps, and an accepted
  journal is never re-walked — so a blob (or now chunk) that failed once on a
  network blip stays unmaterialized until someone edits that path. Pre-existing
  on the whole-blob path (its "next cycle retries" comment overstates what
  happens), inherited unchanged by chunks. A missing-content re-fetch pass over
  the replayed target would close it. Separate change, not delta-sync scope.

## Status

_**LOOP COMPLETE, 2026-08-12.** All 29 rows pass; `go build && go vet &&
go test ./...` green on the full tree (11 packages, incl. the four
real-binary E2E suites and the merge-base old client). Not rows of this loop
and still owed before a PR: README on-disk-layout section, web/docs reference
pages, `architecture/{cli-sync,webapp-server}.md` diagrams (PRD Phase 4), and
the compression follow-up G5 ordered "after chunking"._

| Round | Rows closed | Notes |
|---|---|---|
| 1 | G1 | Counting harness in `internal/syncer/delta_test.go`; verified direct and through a real two-device cycle. |
| 2 | G2 | Baseline measured: 1-byte edit to a 20 MiB file pushes 20,972,098 bytes (file + journal). Seeded-random content so the post-delta bound stays meaningful. |
| 3 | G4 | gzip on a real Go/text corpus (41 files, 441 KiB): **3.4×**. Loop now blocked on human-owned G3 (real-corpus size/churn data) and G5 (go/no-go). |
| 4 | G3 | Snow authorized measuring local mounts: 4 real projects, every journal op. Median file 5–10 KB, 7 files > 4 MiB anywhere, large-file rewrites = 8.6% of all historic push bytes. Numbers in PRD §Phase 0. |
| 5 | G5 | **Snow's call: GO on chunking** (big files can exist; chunking is the binary/large-file answer — CDC is content-agnostic), **compression ships after** as a follow-up (the text-bulk answer, 3.4×; must skip incompressible blobs). Decided against the 8.6% ceiling, knowingly. |
| 6 | H4 | Key space landed: `chunks/`+`manifests/` in `validStoreKey`, list allowlist, put hash check (chunks get key-equals-hash; manifests verbatim — verified by reassembly) — **and every `migrate.go` enumerator in the same commit**, per the known trap. Full webapp/cmd/syncer suites green. |
| 7 | H1 H2 H3 C4 | `RemoteSource.OpenBlob` reassembles from a manifest (spool → verify → backfill → serve; a hostile whole-blob that fails verify is HEALED by the backfill when an honest manifest exists); chunks presign like blobs incl. refuse-existing, manifests always server-relayed. One sec-test control updated: a valid sha may now ask `manifests/<sha>` after `blobs/<sha>` — the guard property (malformed Op.Blob never reaches storage) unchanged, all hostile subtests still pass. Full webapp suite green. |
| 13 | — (CTO round 3: H6) | **The basis machinery is deleted, not patched.** Three client-side skip proxies each proved false or forgeable (local basis; manifest existence; stored manifest content — a member who can READ a file can publish its true hashes without uploading a byte). The only valid proof is asking: pushChunked now does one `Exists` per chunk, and the basisOf/journal-scan derivation in push is gone. Hub ingest enforces "manifest ⟹ chunks exist" (`TestSec_Chunks_ManifestMustNameUploadedChunks`); import enforces the same for archives (`TestDelta_Import_RefusesManifestNamingAbsentChunks`); truthful-squat regression `TestDelta_Basis_TruthfulSquatCannotSkipChunks`. False "blob corrupt on remote" no longer reported when the whole-blob fallback lands the file. Byte counts unchanged. |
| 12 | — (CTO round 2) | Fixed B1 (basis presumed remote — superseded by round 13's deletion), B2 (old export silently loses chunked files → import refuses incomplete archives, `--allow-incomplete` escape), H1 (bad manifest denies good blob → always-fallthrough + write-once manifests), M4 (vacuous upgrade test made honest), M5 (write-once fails closed), M6 (Exists-gated fallback). |
| 11 | — (post-loop hardening) | **Reassembly bound** (`maxReassembleBytes` = 256 MiB, mirrors maxImportBlob): a member-written manifest's declared sum is refused before any chunk fetch, and each chunk's copy is bounded by its declared size so an oversized stored object (replayed presign) can't defeat the cap — `TestSec_Chunks_ReassemblyBoundsHostileManifest`. **Device ceiling raised**: `maxPullBytes` 32 → 100 MiB per Snow (chunking shipped FOR large files; a ceiling below them was dead weight) — pinned by `TestDelta_Ceiling_LargeFileMaterializes` (40 MiB converges). |
| 10 | E1–E6 | **E block closed with real binaries.** Old client built from the pre-change commit via `git archive` (`buildOldBinary`; ref must become the merge-base once this work is committed). E2/E3: old binary syncs chunked-only storage (hub reassembly) and full old↔new round-trip converges. E1 over real HTTP: 1,954,726 pushed / 1,954,696 pulled for a 1-byte edit to 20 MiB. E5: viewer/history/download/anon share all correct. E4: real export → import (import creates the project; the harness's earlier pre-create collided by design). E6: daemon propagates a chunked edit. Three harness fixes en route (stop=pause → cleanup-only; join by `--name`; import owns project creation) — no product changes needed. |
| 9 | C3 C8 | Hostile manifest (individually-valid chunks assembling to the wrong content) surfaces as errBlobContent, never files under the op's sha, never materializes. Chunked-only project round-trips export→import; corrupt chunk in an archive refused like a corrupt blob. All legacy migrate + sec tests green. |
| 8 | D1–D6 C1 C2 C5 C6 C7 | **The chunker landed** (`internal/syncer/chunks.go`, restic/chunker, 256K/1M/4M, fixed Rabin poly; >4 MiB files move as chunks+manifest, basis = previous version of the path on both sides). 1-byte edit: 20,972,098 → **2,050,572 bytes**; front insertion **526,944** (D4, the CDC discriminator); pull-with-basis **632,938**. G2 updated per its own failure message. Full `go test ./...` green incl. daemon/agenthooks. D6 note: on a bare `file://` remote a chunked-only file has no whole blob — cold path is manifest-first by design; hub-served whole-blob cold path is E5's. Lead recorded: transient fetch failures were never retried on the whole-blob path either (journal-growth-triggered only). |
