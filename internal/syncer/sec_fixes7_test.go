package syncer

// Round 8 — the target is round 7's fixes (382882b), and specifically the ONE
// hole round 7 named and declined to close: `redefinesApplied` refuses a peer
// that REDEFINES an applied op's (device, seq) slot, but cannot refuse one
// that simply makes the slot GONE, because round 4's
// TestSec_Pull_APeerCannotChooseWhichOpsEachDeviceSees requires a latecomer
// and an early device to converge after exactly that rewrite.
//
// The CISO's stated clean close is hub-side append-only enforcement on
// /store/*. The second test here shows that is NOT sufficient: the same
// withdrawal is reachable through a byte sequence that IS a pure append.
//
// Helpers are prefixed secfx7; no existing file is touched.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// secfx7Op renders one journal line for device dev. Unlike secfx4Op it takes
// the device, because these attacks turn on WHICH slot a line occupies.
func secfx7Op(t *testing.T, dev string, seq int64, kind, path, blob string, size int) string {
	t.Helper()
	b, err := json.Marshal(journal.Op{
		Seq: seq, Lamport: seq, Time: time.Unix(1767225600+seq, 0).UTC(),
		Device: dev, DeviceName: dev, Kind: kind, Path: path, Blob: blob, Size: int64(size),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// secfx7Deletes reports whether any op in the peer's journal, as the victim
// now holds it, is a delete of path — i.e. whether the file leaving the
// working folder is EXPLAINED by the journal (and therefore by History).
func secfx7Deletes(ops []journal.Op, path string) bool {
	for _, op := range ops {
		if op.Kind == journal.KindDelete && op.Path == path {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 1. The declined hole: an applied op withdrawn by corrupting its line.
// ---------------------------------------------------------------------------

// A file that a teammate already has on disk may only leave that disk because
// somebody DELETED it — an op that says so, in the journal, visible in
// History. That is the whole promise of an append-only per-device log.
//
// A peer owns its own journal object and nothing on the hub makes it
// append-only, so it can republish it. Round 6's guard is an op COUNT and
// round 7's is an op IDENTITY on slots that are still present; neither says
// anything about a slot that has been removed. So the peer:
//
//	v1: seq1 put a.md, seq2 put b.md          <- victim syncs, has both
//	v2: <junk>, seq2 put b.md, seq3, seq4     <- seq1's line no longer parses
//
// len(all)=3 >= len(prev)=2, so the count guard passes; slot (mal,1) is not
// present in `all` at all, so redefinesApplied returns false. The victim
// overwrites its local copy with v2, replay no longer contains a.md, and
// materialize removes it. Nothing in the journal, nothing in History.
func TestSec_Pull_APeerCannotUnpublishAnOpEveryDeviceAlreadyApplied(t *testing.T) {
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", storage)

	blobA := secfx4Blob(t, storage, "the file that must not silently vanish")
	blobB := secfx4Blob(t, storage, "B")
	blobC := secfx4Blob(t, storage, "C")
	blobD := secfx4Blob(t, storage, "D")

	op1 := secfx7Op(t, "mal", 1, journal.KindPut, "a.md", blobA, 1) + "\n"
	op2 := secfx7Op(t, "mal", 2, journal.KindPut, "b.md", blobB, 1) + "\n"
	secfx4Journal(t, storage, "mal", op1+op2)

	cycle(t, victim)
	if got := secfx4Tree(t, victim.Folder); got["a.md"] == "" {
		t.Fatalf("control: the victim never applied a.md in the first place, tree=%v", secfx4Keys(got))
	}

	// The rewrite. seq1's line is replaced by bytes Parse drops, and two more
	// ops are appended so the op count does not fall.
	junk := "{" + string(make([]byte, 0)) + "not json at all\n"
	op3 := secfx7Op(t, "mal", 3, journal.KindPut, "c.md", blobC, 1) + "\n"
	op4 := secfx7Op(t, "mal", 4, journal.KindPut, "d.md", blobD, 1) + "\n"
	secfx4Journal(t, storage, "mal", junk+op2+op3+op4)

	cycle(t, victim)

	after := secfx4Tree(t, victim.Folder)
	if after["a.md"] != "" {
		return // the pull was refused: the secure outcome
	}
	ops, err := journal.ReadFile(victim.Store.JournalPath("mal"))
	if err != nil {
		t.Fatal(err)
	}
	if secfx7Deletes(ops, "a.md") {
		return // it left, but the journal says why — also acceptable
	}
	t.Errorf("a.md left the victim's folder with no delete op to explain it.\n"+
		"  tree now:            %v\n"+
		"  peer journal now:    %d ops, none of them a delete of a.md\n"+
		"A peer republished its own journal object with one already-applied line replaced by "+
		"bytes journal.Parse drops, and appended as many ops as it removed. Round 6's op-count "+
		"guard sees 3 >= 2 and round 7's redefinesApplied sees no CONTRADICTED slot — only a "+
		"MISSING one. The file is gone from every teammate's disk, there is no delete op, and "+
		"History has nothing to show.", secfx4Keys(after), len(ops))
}

// ---------------------------------------------------------------------------
// 2. Hub-side append-only does not close it.
// ---------------------------------------------------------------------------

// Round 7's note says the class "is removed properly only on the hub —
// /store/* refusing a journal PUT that is not an extension of the stored
// object". This test does exactly that withdrawal through a PURE APPEND: every
// byte the hub already held is still a prefix of what the peer publishes, so
// an append-only hub accepts it without objection.
//
// The lever is that journal.Parse does not require a trailing newline, which
// pull's own comment already acknowledges ("an op published unterminated is
// APPLIED while sitting outside the prefix HasPrefix protects"). A peer
// publishes its last op UNTERMINATED. The victim applies it. The peer then
// appends bytes that fuse onto that line and make it undecodable — a strict
// extension of the object, and the applied op is withdrawn.
//
// The test asserts the same property as the one above, so it goes green under
// any fix that keeps an applied op applied, and it stays as the standing proof
// that a byte-level append-only rule on the hub is not that fix.
func TestSec_Pull_AnAppendOnlyJournalCannotUnpublishAnAppliedOp(t *testing.T) {
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", storage)

	blobA := secfx4Blob(t, storage, "the file that must not silently vanish")
	blobB := secfx4Blob(t, storage, "B")

	// v1 ends WITHOUT a newline — a peer chooses the bytes of its own object.
	v1 := secfx7Op(t, "mal", 1, journal.KindPut, "a.md", blobA, 1)
	secfx4Journal(t, storage, "mal", v1)

	cycle(t, victim)
	if got := secfx4Tree(t, victim.Folder); got["a.md"] == "" {
		t.Fatalf("control: the victim never applied a.md in the first place, tree=%v", secfx4Keys(got))
	}

	// v2 = v1 ++ more bytes. Nothing already published is altered or removed,
	// so this is an append by any byte-level test the hub could apply — the
	// suffix simply lands on the same LINE and stops it decoding.
	v2 := v1 + `,"trailing":` + "\n" + secfx7Op(t, "mal", 2, journal.KindPut, "b.md", blobB, 1) + "\n"
	if len(v2) <= len(v1) || v2[:len(v1)] != v1 {
		t.Fatalf("control: v2 is not an extension of v1")
	}
	secfx4Journal(t, storage, "mal", v2)

	cycle(t, victim)

	after := secfx4Tree(t, victim.Folder)
	if after["a.md"] != "" {
		return
	}
	ops, err := journal.ReadFile(victim.Store.JournalPath("mal"))
	if err != nil {
		t.Fatal(err)
	}
	if secfx7Deletes(ops, "a.md") {
		return
	}
	t.Errorf("a.md left the victim's folder with no delete op, and the peer's journal object "+
		"was only ever APPENDED to.\n"+
		"  tree now:         %v\n"+
		"  peer journal now: %d ops, none of them a delete of a.md\n"+
		"The peer published its op unterminated, the victim applied it, and the next append "+
		"fused onto that line. A hub enforcing byte-level append-only on /store/* accepts this "+
		"exact sequence — so append-only on the hub is not the fix for this class.",
		secfx4Keys(after), len(ops))
}

// ---------------------------------------------------------------------------
// 3. Control: the hub really would see this as an append.
// ---------------------------------------------------------------------------

// Not an attack — it pins the claim the test above rests on, so a future
// reader does not have to take it on trust: the two objects the peer publishes
// stand in a strict prefix relation, which is the only thing a byte-level
// append-only rule on /store/* can check.
func TestSec_Pull_TheUnterminatedRewriteIsAByteLevelAppend(t *testing.T) {
	v1 := secfx7Op(t, "mal", 1, journal.KindPut, "a.md", "deadbeef", 1)
	v2 := v1 + `,"x":` + "\n" + secfx7Op(t, "mal", 2, journal.KindPut, "b.md", "cafe", 1) + "\n"

	if got, _ := journal.Parse([]byte(v1)); len(got) != 1 {
		t.Fatalf("v1 parses to %d ops, want 1 — the victim must APPLY the op first", len(got))
	}
	got, _ := journal.Parse([]byte(v2))
	for _, op := range got {
		if op.Path == "a.md" {
			t.Fatalf("v2 still yields a.md; the withdrawal premise does not hold")
		}
	}
	if len(v2) <= len(v1) || v2[:len(v1)] != v1 {
		t.Fatalf("v2 is not a strict extension of v1")
	}
}

var _ = remote.Object{}
