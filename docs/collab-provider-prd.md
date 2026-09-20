# PRD: Let the hub hold the document

Co-editing works. What surrounds it does not: every bug this area has produced
comes from the same root, which is that **nobody owns the document**. The CRDT
lives only between browsers, the hub is a relay that never looks inside a
frame, and the file is written by whichever client stopped typing last.

That design was correct when it was chosen, for a reason written into
`collab.go`: a cgo y-crdt "would break the cross-compiled release the same way
a cgo sqlite would". **That constraint no longer holds** — pure-Go, CGO-free
Yjs ports now exist and embed as an `http.Handler`.

This spec replaces the hand-rolled provider with a server-held document, and
deletes the machinery that exists only to compensate for not having one.

Nothing here changes the journal, `journal.Less`, `Replay`, or any sync
invariant. The file stays the artifact.

## What "nobody owns it" has cost

Every item below is a real failure, not a hypothetical. Dates are 2026-09-18.

**1. A client that loses the relay overwrites everyone.**
`CollabDoc.onUnavailable` falls back to single-writer editing over its own
buffer. Two browsers then hold two documents and take turns PUTting them
wholesale. Reproduced in `e2e/concurrent-edit.spec.ts`: six characters typed
by one editor vanished, no conflict copy, no warning. A real history feed
showed it as versions alternating between ~1.3 KB and ~750 B several times a
minute — for a user whose network had just changed.

Mitigated (not fixed) by the `If-Match` + conflict-copy work on
`fix/editor-conflict-copies`. A conflict copy is the right safety net; needing
one every time a laptop changes network is not.

**2. Seeding is a claim, with a grace timer.**
`seedClaimGrace` (10 s) exists because a Yjs document seeded independently by
two clients is not the same document — merging them duplicates every
character. So the room tells exactly one joiner it is first, and a claim that
never produces anything has to expire. A server that holds the document has
nothing to claim.

**3. The room log grows without bound, then everyone rebuilds.**
`maxRoomBytes` (8 MiB) is memory a member can grow by typing. Past it the room
empties and every client reconnects — and the next seeder seeds from
`seedText`, captured when its editor opened, which may be hours old.

**4. Reconnect replays the whole log.**
`reconnect()` builds a fresh `Y.Doc` and re-applies every update in the room,
because the provider has no state-vector handshake. Long sessions pay for
their own history on every flaky connection.

**5. Discovery is a chicken-and-egg.**
Awareness is relayed and never logged, so an announcement is lost to anyone who
arrives later. Suppressing cursor traffic when alone (the obvious fix for
one-POST-per-arrow-key) makes two people permanently invisible to each other
unless a re-announce is bolted on — which it now is, and which a server that
knows who is in the room would not need.

**6. Snapshotting is a client's job.**
"Whoever stops typing last writes the file" means N co-editors each write the
same converged text: N journal ops, N change frames, N history rows, N full
tree refetches for every other client. See
`docs/network-efficiency-prd.md` stages 3 and 4, which are partly cleaning up
after this.

Items 2, 3, 4, 5 and 6 are not bugs to fix. They are **compensations for the
missing owner**, and they disappear with it.

## What changed outside this repo

Pure-Go Yjs ports, no CGO, wire-compatible with `yjs@13.x`:

- [Deln0r/ygo](https://github.com/Deln0r/ygo) — byte-for-byte V1/V2 wire
  compatibility with cross-language fixtures; its `yserve` is a single-binary
  Hocuspocus-compatible server that **also embeds as a plain `http.Handler`**,
  which is what keeps our auth, org walls and `proj()` permission wrapper in
  front of it.
- [reearth/ygo](https://github.com/reearth/ygo) — versioned persistence over
  `modernc.org/sqlite`, the same pure-Go driver `db_sql.go` already uses. Ships
  with **no built-in auth** and binds loopback by default, which is fine
  embedded and dangerous standalone.
- Both are listed on the [Yjs ports page](https://docs.yjs.dev/ecosystem/ports-to-other-languages).
- [y-sweet](https://github.com/jamsocket/y-sweet) is the more established
  S3-backed option and is rejected here only because it is a second binary to
  deploy, which costs the single-binary self-hosting story.

On the client, `@hocuspocus/provider` replaces our provider with a maintained
one (reconnect, backoff, state-vector sync). The repo already depends on
`@tiptap/*`, so this is the native pairing rather than a new ecosystem.

## The decision this needs first

**Today the hub never parses a client-supplied CRDT update.** `collab.go`
stores opaque bytes and hands them on. Embedding a CRDT ends that property:
untrusted bytes get decoded, in-process, in the hub.

That is a security-posture change, not a dependency bump, and it is the one
call that cannot be made by whoever picks up this document. Decide it
explicitly, in §Status, before Stage 1.

Arguments for: every other input the hub parses (journals, JSON, markdown,
uploads) is already untrusted, and `store.go` already inflates client-supplied
gzip under a bounded reader. Arguments against: a CRDT decoder is a much larger
attack surface than any of those, in a young library, reachable by any project
member.

## Goals

- A disconnected client cannot overwrite a teammate's work, by construction.
- One editing session writes one file version per settle, not one per editor.
- A reconnect transfers what the client is missing, not the session's history.
- `collab.ts`'s bespoke provider and the compensations above are **deleted**,
  not extended.

## Non-goals (do NOT do these)

- Not a CRDT change. Yjs stays; nothing that failed was a Yjs bug.
- The file remains the artifact. Agents, the CLI and the desktop write files
  and never touch a CRDT — any design that makes the document authoritative
  over the file is wrong for this product.
- **Keep `If-Match` and conflict copies.** They protect the file from every
  writer, including the ones with no room to join. This work should make them
  rare, never unnecessary.
- No Node runtime, and no second binary to deploy.
- No hosted service (Liveblocks, Tiptap Cloud, PartyKit): self-hosting is the
  product.

## Stages

### Stage 0 — decide, then spike

- [x] The parsing-untrusted-updates decision, recorded in §Status with the
      conditions it carries
- [x] **Port chosen: `github.com/reearth/ygo` v1.50.0.** CGO-free; ships
      `crdt`, `awareness`, `provider/websocket`, `persistence` and `cluster`
      (which is the multi-process answer, not a gap); 50 minor releases and
      its own JS-compat suite in-tree. `Deln0r/ygo` was the other candidate
      and is also credible — same wire claims, a Hocuspocus-compatible
      `yserve` — and is the fallback if this one stalls.
- [x] **Cross-language fixtures pass, against the exact `yjs` the browser
      ships** (`node_modules/yjs`), both directions and both encodings:

      | | V1 | V2 |
      |---|---|---|
      | JS reads a Go document (`ünïcode ✅`) | MATCH | MATCH |
      | Go reads a JS document (`日本語 🎉`) | MATCH | MATCH |
      | Go reads a JS document of 10,000 ops | MATCH | MATCH |

      These move into CI in Stage 1 as a Go test with checked-in fixtures, so
      a version bump that breaks the wire fails the build rather than the
      editor.
- [x] **Memory per open document measured** (`crdt.New()` + `YText`, heap
      delta after GC, 50-200 documents per shape):

      | shape | per document |
      |---|---|
      | 10 KB file inserted whole (loading a file) | 11.5 KB |
      | 100 KB file inserted whole | 105.5 KB |
      | 10 KB file typed **character by character** | **101.7 KB** |

      The third row is the planning number, and it is the surprise: editing
      costs ~10x the content, because every keystroke is its own item until
      GC merges them. So an actively-edited document is ~100 KB and a hub
      with 100 of them open is ~10 MB — against today's relay, which caps
      each room's update log at 8 MiB on its own.

      **The cap is therefore eviction, not bytes.** `maxRoomBytes` existed
      because an append-only log grows without bound while a document does
      not: the same text typed twice is one document and two log entries.
      Idle documents are dropped the way `roomIdle` drops idle rooms, and the
      file is the durable copy either way.
- [x] A hub running multiple processes: `ygo/cluster` exists for exactly this.
      Scope for Stage 1 is a single process with the cluster path unused and
      named as the upgrade, rather than pretending the question does not
      exist.

**Success criteria:** a written go/no-go with the fixture results and the
memory number in it. **GO.** Fixtures pass, ~100 KB per actively-edited
document, and the cap is an eviction policy rather than a byte ceiling.

### Stage 1 — the hub holds the document

- [ ] The Go Yjs server embedded as an `http.Handler`, mounted behind the
      existing `proj()` wrapper so folder permissions, org walls and read-only
      membership apply unchanged
- [ ] `filterJournal`'s sibling question answered: a reader who cannot see a
      path must not receive its document
- [ ] Documents persist across a hub restart, or are rebuilt from the file
      deterministically
- [ ] The old relay stays behind a config flag for one release

**Success criteria:** two browsers converge through the hub with the bespoke
provider deleted from the path; `e2e/concurrent-edit.spec.ts` still passes.

### Stage 2 — the client stops being a provider

- [ ] `@hocuspocus/provider` replaces `CollabDoc`
- [ ] `collab.ts` deleted, not adapted
- [ ] `onSolo` and every caller of it deleted — there is no solo mode when the
      server holds the document; an unreachable hub is offline, and offline
      editing is the desktop app's job, not a second CRDT path

**Success criteria:** `zz-stress-edit.spec.ts` — three editors, one cut off
throughout, one losing the relay halfway — produces **zero conflict copies**,
where today it produces them by design. Reconnect transfers less than the
session's total update volume (measured, not assumed).

### Stage 3 — the server writes the file

- [ ] The hub snapshots the document to the file on settle, replacing
      "whoever stops typing last writes it"
- [ ] One settle = one journal op, whatever the number of editors
- [ ] `Op.User` still names the human, not the hub — a version whose author is
      "the server" is a regression in History

**Success criteria:** a three-editor session that today produces one version
per editor per pause produces one per pause. Measured against
`docs/network-efficiency-prd.md`'s history-noise baseline.

### Stage 4 — delete the compensations

- [ ] `seedClaimGrace`, the seed claim, `maxRoomBytes`/`full`, and the
      reconnect-and-replay path removed
- [ ] The awareness announce/re-announce dance removed — the server knows the
      roster
- [ ] `architecture/webapp-server.md` and `webapp-frontend.md` updated; the
      `collabRoom` notes are the largest single block of "why this is hard" in
      either diagram and most of it should stop being true

**Success criteria:** net lines deleted, and no new mechanism replacing one
that went away.

## Risks

- **Young libraries.** Mitigated by Stage 0's fixtures and by keeping the old
  relay behind a flag for a release.
- **Untrusted parsing.** The Stage 0 decision; bound the decoder's inputs the
  way `maxInflatedPut` bounds gzip.
- **Memory.** A held document per open file is state the relay never had.
- **Horizontal scaling.** The managed hub may run more than one process; two
  instances holding one document need a backplane or sticky routing. Answer it
  in Stage 0 or scope it out loudly.

## Status

_Stage 0's decision is made (below): the hub may parse client-supplied CRDT
updates. Implementation proceeds._

| Stage | State | Notes |
|---|---|---|
| 0 — decide + spike | **done — GO** | reearth/ygo v1.50.0; JS<->Go fixtures pass V1+V2 incl. 10k ops; ~100 KB per edited doc |
| 1 — hub holds the document | next | |
| 2 — client stops being a provider | blocked on 1 | |
| 3 — server writes the file | blocked on 1 | |
| 4 — delete the compensations | blocked on 2, 3 | |

### The decision

- [x] **May the hub parse client-supplied CRDT updates?** **Yes.**
      Decided by: Snow Lee. Date: 2026-09-19.

      Conditions carried forward into Stage 0 and Stage 1 rather than left as
      a sentiment: the decoder's inputs are bounded the way `maxInflatedPut`
      bounds gzip, the server is mounted behind the existing `proj()` wrapper
      so folder permissions and org walls apply unchanged, and the old relay
      stays behind a config flag for one release.
