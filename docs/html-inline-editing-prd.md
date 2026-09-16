# PRD: Inline HTML editing — click the text, type

Today an `.html` file in the viewer is read-only markup in a sandboxed iframe.
The Edit button works on it, but it drops you into CodeMirror on the **source** —
which is the right tool for markup and the wrong one for fixing a typo in a
headline an agent wrote.

This spec adds **click-to-edit**: in edit mode, clicking a paragraph on the
rendered page makes that paragraph editable in place, with the page's own CSS,
and Cmd+B / Cmd+I doing what they do everywhere else.

The hard part is not the editor. It is that the file on disk must stay the file
an agent wrote — same indentation, same comments, same `<script>` — so that the
next agent to read it sees what it left, and the history diff is one line rather
than a whole-file rewrite.

Implement the phases in order; a phase is done only when every acceptance box in
its checklist is checked. Record progress and blockers in §Status.

## Decisions (settled, do not relitigate)

| Question | Answer |
|---|---|
| What lands in the file | **Surgical patch.** One element's inner range is replaced in the source string. Everything outside it is byte-identical. |
| Which files | **Any synced `.html`**, gated by the same write permission (project + folder) the Edit button already resolves. |
| Which text is clickable | **Text that exists literally in the source.** JS-generated DOM has no source range, so it is simply not clickable. The rule falls out of patching; it is not a heuristic. |
| Render surface | **Keep the sandbox.** The iframe stays `allow-scripts` with no `allow-same-origin`; the editor talks to the parent over `postMessage`. |
| Editing surface | **TipTap** scoped to one element at a time. |
| Extensions | **Bold, Italic, Code**, plus a passthrough mark for every other inline tag. NOT TipTap's Link — it re-renders anchors with its own `target`/`rel` defaults, which the file never had. |
| Markup TipTap can't model | **Carried through verbatim** by a passthrough mark, with the round-trip check still the arbiter. Originally this refused the element outright; real use showed that refuses most prose, since almost every paragraph has a link or a span in it. |
| Structural keys | **Enter and Backspace do nothing at block edges.** One element in, one element out. |
| Paste | **TipTap's own handling.** This is the main thing the library is being paid for. |
| Co-editing | **Via the source `Y.Text`** — the same CRDT document the source editor already binds to. No rich-text CRDT. |
| Page scripts | **Left running.** A `MutationObserver` warns if the page overwrites an edited node. |
| Discoverability | **Edit button first**, then hover outlines on editable regions. |
| Rollout | **On by default.** See §Risks for why this is the uncomfortable one. |

## The design in one picture

```
              Y.Text  =  the .html source            ← one CRDT, two views
                 │                    │
        CodeMirror                iframe ?edit=1
        (Edit source)             (click-to-edit)
                                       │
                                  postMessage
                                  {srcRange, html}
                                       │
                                  splice Y.Text
```

The insight that makes this cheap: **the CRDT stays a plain-text CRDT of the HTML
source.** It is the document `Editor.tsx` already shares. A visual edit is a text
splice into it. Two consequences fall out for free:

- The "Edit source" escape hatch and the visual editor co-edit *each other*.
  Someone in CodeMirror and someone clicking a headline are in the same room.
- The save path, the conflict banner, the quota check and the journal op are all
  the ones that exist. Nothing downstream learns that a visual editor happened.

### Server: `?edit=1`

`internal/webapp/server.go` already appends a script to served HTML for
`?print=1` (`printSuffix`, and `printView` relaxes the sandbox by exactly one
flag). `?edit=1` is the same shape:

1. Require write permission on the path — the same `writablePath` the save door
   uses. A reader gets the plain render.
2. Tokenize the source with `golang.org/x/net/html` (**already an indirect dep**;
   `Tokenizer.Raw()` gives offsets by accumulation).
3. Stamp `data-bd-src="<start>,<end>"` on every **innermost text-bearing
   element** — offsets delimiting its *inner* content in the **original**
   source, recorded before any stamping shifts anything, and counted in
   **UTF-16 code units** because that is how the browser will index them
   (`utf16Len`; see §Status for what counting bytes cost).
4. Append `<script src="/inline-edit.js" data-src-hash=… data-src-len=…>`. At
   the static root, not under `assets/`, which is served immutable for a year.

Stamping rule — an element is stamped when all hold:

- not inside `head`, `script`, `style`, `template`, `pre`, `textarea`, or `svg`
- it is not itself **phrasing content** (`inlineElements`): an `<a>` or
  `<strong>` is part of the prose around it, hands its text to its parent, and
  never becomes a region of its own
- contains at least one non-whitespace text token
- has **no stamped descendant** (computed bottom-up, so `<p>` is stamped and
  `<body>` is not)

`pre`/`textarea` are excluded at stamp time rather than left to the round-trip
check: they are raw-text contexts where whitespace is load-bearing and a
paragraph schema will mangle it.

Not served on `/s/*` share pages and not on the download doors — same rule
`printView` follows, for the same reason.

### Client: the iframe bootstrap

`inline-edit.js` is a **separate Vite entry**, not part of the main bundle.
Readers never download TipTap; it arrives only when someone enters edit mode.
A sandboxed opaque-origin iframe can load a **classic** script cross-origin
without CORS, so `src="/inline-edit.js"` works with `allow-same-origin` still
off — but a MODULE could not, so this entry is built as an IIFE in its own Vite
pass (`vite.inline-edit.config.ts`).

On load it posts every `data-bd-src` range up to the parent. On click:

```js
const el = e.target.closest("[data-bd-src]");
// The gate. Schema loss is caught here, before anything is editable.
if (serialize(parse(el.innerHTML)) !== el.innerHTML) return markUneditable(el);
mountEditor(el); // ProseMirror EditorView {mount: el}, NOT TipTap's Editor
```

On idle (700 ms, matching `SAVE_IDLE_MS`) or blur, post `{srcRange, html}` up.

### Parent: offsets → relative positions → splice

Absolute offsets go stale the instant a peer edits. Yjs has the answer already:

1. On receiving the range list — and not before the shared document is actually
   seeded — verify `sha256(Y.Text.toString())` matches the page's
   `data-src-hash`. Mismatch ⇒ the document moved between render and load ⇒
   reload the iframe. Where `crypto.subtle` is missing (a plain-HTTP hub is not
   a secure context) fall back to comparing `data-src-len`.
2. Convert every `(start, end)` to a pair of `Y.RelativePosition`. These survive
   concurrent edits — this is exactly what they exist for, and the Yjs docs
   warn about index positions specifically.
3. On a patch, resolve back to absolute indices and splice `Y.Text`.

Because the iframe is opaque-origin, its `event.origin` is `"null"` and cannot be
checked. **The parent must verify `event.source === iframe.contentWindow`**
instead. Any other message is dropped.

### Known ceiling: normalization inside the edited element

The round-trip check compares TipTap's output against the browser's
`innerHTML`, not against the source slice — comparing against the source would
fail constantly on entity and attribute-quoting differences the browser
normalizes on parse.

So: **the edited element's inner markup may come back normalized**
(`&nbsp;` → literal nbsp, attribute quoting, self-closing form). Everything
outside that element is byte-identical.

This is a deliberate, bounded corner: the blast radius is one element, never the
file. Document it in `reference/` rather than pretending it isn't there.

## Phases

### Phase 1 — Server stamping

- [x] `?edit=1` gated on write permission; reader gets the plain render
- [x] Offsets are against the original source and survive stamping
- [x] Exclusions hold: `head`, `script`, `style`, `template`, `pre`, `textarea`, `svg`
- [x] Only innermost text-bearing elements stamped — `<body>` never is
- [x] `bd-src-hash` matches the stored bytes
- [x] Never emitted on `/s/*` or on a download; `?print=1` unaffected
- [x] Malformed HTML does not panic and does not emit bad offsets
- [x] Go test: for a fixture file, every stamped range slices to that element's
      inner source exactly

### Phase 2 — Editing surface, no writes

- [x] `inline-edit.js` is a separate entry; main bundle size unchanged
- [x] Click mounts TipTap with Bold/Italic/Link/Code only
- [x] Round-trip gate refuses elements it would damage — verified against a
      fixture containing `<span class>`, `<sup>`, and a nested `<div>`
- [x] Cmd+B / Cmd+I / Cmd+Z behave
- [x] Enter and Backspace at block edges are inert
- [x] Paste from a rich source lands as Bold/Italic/Link/Code or plain text
- [x] Parent verifies `event.source`; a message from anywhere else is dropped
- [x] Playwright: click → type → intended patch is reported, nothing written

### Phase 3 — Patch and save

- [x] `bd-src-hash` mismatch reloads rather than patching
- [x] Ranges held as `Y.RelativePosition`
- [x] Splice lands in `Y.Text`; existing save path writes the file
- [x] Go test: one-sentence edit produces a one-element diff; indentation,
      comments and `<script>` blocks are byte-identical
- [x] Folder-level read-only refuses server-side, and the Edit button never
      appears for it (the "button that 403s" rule)

### Phase 4 — Collaboration and live pages

- [x] Two browsers editing different paragraphs converge
- [x] One in CodeMirror, one clicking — both land, both see each other
- [x] A remote edit re-renders the iframe unless the focused element is dirty
- [x] `MutationObserver` warns when the page overwrites an edited node
- [x] Existing `#peer-wrote` banner still fires for outside writes

### Phase 5 — Surface and docs

- [x] Edit mode shows hover outlines; uneditable regions show why on hover
- [x] "Edit source" escape hatch reachable from edit mode
- [x] `README.md`, `web/docs/.../reference/` updated, including the
      normalization ceiling and the JS-generated-content rule
- [x] `npm run build` committed; `frontend/check-dist.sh` clean

## Non-goals

- Structural editing — adding, deleting, reordering or splitting blocks. Enter
  is inert; the answer is Edit source.
- Images, tables, headings-as-structure, lists-as-structure.
- Rich text beyond Bold / Italic / Link / Code.
- Editing text a page's JavaScript generated. It is not in the file.
- Editing through a share link.
- Mobile / touch. Desktop mouse and keyboard only in v1.
- Markdown files. They have an editor already.

## Risks

**Shipping on by default is the live one.** A round-trip bug does not show up as
a broken page — it shows up as a teammate's file quietly losing a `<span>`, then
syncing that loss to every device. The round-trip gate is the primary defense
and it is checkable rather than hoped-for, which is why it is worth the elements
it makes read-only.

The mitigation that already exists: **blobs are retained forever and history has
a restore path.** A corrupted file is one restore away, and `/history` shows
exactly which save did it and who was signed in. That is a real backstop, not a
consolation — but it is recovery, not prevention, and it only helps if someone
notices.

If Phase 3's diff test is anything other than boringly green, stop and add the
config flag before merging.

**Secondary:** an HTML file carrying its own `<meta http-equiv="Content-Security-Policy">`
can block `inline-edit.js`. Rare, self-inflicted, and it degrades to "clicking
does nothing" rather than to damage. Detect and fall back to Edit source.

## Status

Phases 1–5 implemented.

- `go test ./...` — green.
- `e2e/inline-edit.spec.ts` — 11 tests green, stable across repeats.
- Full hub e2e — **237 passed, 1 failed**. The failure is `shell.spec.ts:10`,
  which fails identically on a clean `main` worktree; it is not this branch's.
  (Verified by building `main` in a throwaway worktree and running the same
  suite: 226 passed, the same 1 failed.)

Every non-trivial rule here was mutation-tested — the rule was broken on
purpose to confirm a test fails. The range arithmetic, the innermost rule, the
inline-content rule, the too-large put-back and the UTF-16 offsets all have a
test that dies without them.

### What the build taught us that the spec had wrong

Four things this document asserted turned out to be false, and each cost a
round of debugging. They are recorded here because the next person to touch
this will hit the same ground.

**The innermost rule ate itself.** "Innermost text-bearing element" stamps the
`<a>` inside a sentence and then marks its `<p>` as already-covered — so the
only editable thing in "See the [notes] for the method" was the link text. Real
prose nearly always contains a link or an emphasis, so almost nothing was
editable. Phrasing content had to become *content*: an inline element hands its
text to its parent and disappears (`inlineElements`, and the `pop` branch above
it). This is also what makes the round-trip check do the job it was designed
for — the paragraph goes to the editor whole, `<a>` passes, `<span class>`
refuses.

**TipTap's `Editor` could not be used.** It mounts by appending a child, which
puts a `<p>` inside an `<h1>`. The editor is ProseMirror's `EditorView` with
`{mount: el}`, using TipTap only for the schema (`getSchema`) — which is the
part worth having: the parse/serialize rules for bold, italic, link and code.

**The bootstrap cannot be an ES module.** A sandboxed iframe with no
`allow-same-origin` has an opaque origin, so a module script would be a
cross-origin module load needing CORS the hub has no business growing. It is
built as a classic IIFE in its own Vite pass (`vite.inline-edit.config.ts`).

**Anchoring had a race the spec did not see.** The iframe's `ready` can arrive
before the relay seeds the shared document, and a relative position taken
against an EMPTY `Y.Text` resolves to index 0 — so every anchor silently
collapses onto the start of the file. Anchoring now requires a non-empty
document and retries on `text.observe`. This was intermittent, and it is the
one failure here that would have corrupted a real file rather than just not
working.

**Offsets are UTF-16 code units, not bytes.** The server counted bytes; the
browser resolves them against a `Y.Text`, which — like every JS string — is
indexed in UTF-16 units. ASCII makes the two identical, so every fixture in
this repo passed while the feature would have corrupted the first paragraph of
any file written in French, Japanese, or anything else with a non-ASCII
character in it. `utf16Len` counts the way the client does. Both the Go table
and an end-to-end spec now use non-ASCII fixtures, and the e2e one includes an
emoji specifically: 4 bytes, TWO UTF-16 units, so "count runes instead" fails
it too.

The first version of that e2e test was worthless and passed the mutation. With
byte offsets the range resolves past the end of the document and the patch is
APPENDED after `</html>` — where the file still contains the edited text and
every neighbour is intact. Asserting a substring proved nothing; asserting the
text inside its own tags, plus that the file still ends where it should, is
what catches it.

**`crypto.subtle` does not exist on a plain-HTTP hub.** It is secure-context
only, and a LAN-bound hub over HTTP is a supported deployment. The hash check
would have thrown there and left click-to-edit permanently inert, with no
error anyone would see. The server now also stamps the source length, and the
client falls back to comparing that — weaker (it misses a length-preserving
edit) but real, and the difference between a safety net and none.

**Seed data is shared state, and growing it is not free.** The e2e fixture
started as one HTML file added to the seeded `wiki` project. That single extra
row destabilised EIGHT unrelated specs: it shifts the shell's timing enough to
expose a latent bug in the account menu (after renaming the org it takes two
clicks to reopen — `AccountBar.tsx`, nothing to do with editing, and present on
`main`). Writing the files per-test instead fixed the admin specs but broke four
`session-run` ones, and removing them afterwards broke those differently — the
removals are themselves entries in the change feed. The spec now works in a
project of its own and touches `wiki` not at all.

That latent account-menu bug is still there, unfixed and unowned by this work.
It reproduces on `main` by adding any file to the seed. Worth a ticket.

### Round two: what real use found that the tests did not

The suite was green and the feature was broken in three visible ways the moment
a person used it. Each one is now pinned by a test that fails without the fix.

**Every edited paragraph vanished off the screen.** ProseMirror's `destroy()`
ends with `dom.textContent = ""` when the view was created with `{mount}` — it
assumes the owner re-renders, and nothing re-renders the page's own markup here.
Clicking from one paragraph to the next wiped the first one. The FILE was
correct throughout, which made it worse: it looked exactly like the editor
eating the document. `unmount` now puts the final markup back.

**One edit replaced the ENTIRE FILE with one paragraph.** The anchoring guard
read `file.current()`, which falls back to the seed text when the CRDT is empty
— the precise state it existed to catch. It reported a healthy document while
the `Y.Text` being measured held nothing, both positions collapsed, and the
first patch spliced over everything. It reads the CRDT itself now, and a
separate guard refuses any patch whose resolved range would swallow a document
the stamped range was only part of. Dropping an edit is a bad afternoon;
deleting a file is not recoverable from the UI.

**The "someone else changed this file" banner fired on your own edit.** The
flag was set after the PUT resolved, but the change stream announces a write as
soon as the hub journals it — often first. And it was a boolean, so with two
saves in flight (routine: clicking between paragraphs saves each) the second
event had nothing to claim it. It is now a counter, incremented before the
write goes out.

**Most prose could not be edited at all.** Refusing any element whose markup the
schema cannot model sounded safe and was nearly useless: almost every real
paragraph contains a link or a span. Unknown inline tags are now carried through
verbatim by a passthrough mark — the round-trip check remains the arbiter, so
this only ever widens what is editable. Links in particular were refused because
TipTap's `Link` extension re-renders an anchor with its own `target="_blank"`
and `rel="noopener noreferrer nofollow"`; dropping that extension and letting
the passthrough carry `<a>` fixed it and removed 28 KB of bundle.

### Round three: "I clicked Done and nothing happened"

Two independent bugs wearing one symptom, and the first is the important one.

**The iframe debounced its patch by 700ms.** For that window the edit existed
ONLY inside the iframe — and Done tears the iframe down. Type, press Done, edit
gone, file untouched, no error. The debounce was cargo-culted from the save
timer and bought nothing: a patch is a postMessage inside the same tab, and the
debounce that actually matters is the app's save timer, which still batches the
writes that reach the hub. Removed; the patch now goes out on every change.

Every test in the file waited before asserting, which is exactly why this
shipped. The regression test now types and presses Done with nothing in
between — and it caught a second thing when I wrote it.

**The rendered page never reloaded.** An iframe loads once. Leaving the editor
mounts the read view IMMEDIATELY — before the save has landed — so it rendered
the pre-edit file and then sat there, edit saved on the hub and invisible on
screen. It follows the change stream now, which also fixes the quieter version:
a teammate's edit used to never appear in a rendered page you were looking at.

**A room can hand out a permanently empty document.** `leave()` releases a seed
claim when the last subscriber goes, but a stream that is never cleanly torn
down leaves a phantom subscriber, and the room stays claimed and empty forever.
Everyone after that is told they are not the seeder and gets a blank document:
the visual editor silently refuses to save, and the SOURCE editor would mount a
blank buffer and snapshot that blankness over the file. The hub now expires a
claim that produced nothing (`seedClaimGrace`), and the client retries once past
that grace before showing a banner.

The file-render response also carries `Cache-Control: no-cache` now. It had an
ETag and no freshness directive, which leaves a browser free to serve a stored
copy without asking.

### Process notes, because both cost real time

**A silent build failure sent me chasing a phantom.** `npm run build` is
`tsc && vite build`, so a single unused variable leaves the previous bundle in
place — and I had piped the output to `/dev/null`. I spent several rounds
debugging a stale bundle, and later "verified" a mutation test that had never
been built. Check the exit code.

**Silent build failures bit twice.** The second time, a mutation test "passed"
against a bundle that had never been rebuilt, which would have recorded a
guarantee that does not exist. Check the exit code every time.

**A speculative fix to an unrelated component cost six tests.** Chasing a
flaky admin spec, I reordered the account menu's close-then-navigate. It passed
in isolation — which proved nothing, since those specs pass in isolation either
way — and broke six tests in the full suite. Reverted. The account-menu
fragility is real, pre-existing, and still unowned; it is not this feature's to
fix on a hunch.

### Still open

- **The length fallback is weaker than the hash.** On a plain-HTTP hub, a peer
  edit that preserves the document's exact length between render and load
  would not be detected, and that patch would land on stale ranges.
- **Caret placement relies on `posAtCoords`.** Clicking places the caret
  correctly, but the editor is created *after* the click, so the browser's own
  selection is discarded and re-derived. It is right in every case tested; it
  is not the same thing as never having lost it.
- **Rollout went out on by default**, against this document's recommendation.
  The Phase 3 diff test is green, which was the stated condition.
- **Mobile is untested.** Not a non-goal that was verified, just one that was
  never exercised.
- **`<b>` and `<i>` in a source file are still refused.** Bold and Italic parse
  them and render `<strong>`/`<em>`, so the round trip fails and the paragraph
  will not open. Agents overwhelmingly write the semantic tags, so this has not
  bitten — but a hand-written file may hit it, and the answer today is
  Edit source.
- **The teardown drain is unproven.** VisualEdit delays removing its message
  listener by 300ms so an in-flight patch still lands. Removing it fails no
  test — with the debounce gone the patch is delivered long before Done. It
  stays because what it guards is silent, not because it was shown to be
  needed.
