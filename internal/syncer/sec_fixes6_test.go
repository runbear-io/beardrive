package syncer

import (
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Round 7 — attacking round 6's fix for the journal-rewrite undo (row 15).
//
// Round 5 replaced pull's op-count cursor with a byte offset. Round 6 found
// that a peer could then UNDO an already-applied op by rewriting its journal
// object, and added a guard:
//
//	prev, perr := journal.Parse(accepted)
//	all,  aerr := journal.Parse(data)
//	if perr != nil || aerr != nil || len(all) < len(prev) { continue }
//
// The guard counts ops. It does not check that the ops we already applied are
// still IN the object.

// secfx6Del is a delete op for a path nothing ever created: decodable, counted
// by journal.Parse (kind is "delete", so round 5's phantom-op filter passes
// it), and completely inert when replayed. It is padding with a valid ticket.
func secfx6Del(seq int64, dev, path string) journal.Op {
	return journal.Op{
		Seq:     seq,
		Lamport: seq,
		Time:    time.Date(2026, 8, 1, 12, 0, int(seq), 0, time.UTC),
		Device:  dev,
		Kind:    journal.KindDelete,
		Path:    path,
	}
}

// TestSec_Pull_AnInertOpCannotBuyAPeerTheRightToUndoAnAppliedOp is round 6's
// guard, defeated by counting.
//
// Round 6's own reproducer replaced the victim op's line with junk that
// decoded to NOTHING, so the object lost an op and len(all) < len(prev)
// stopped it. The peer does not have to lose an op: it replaces the line with
// a decodable one that does nothing at all — a delete of a path that never
// existed. The count is preserved, the guard is satisfied, the object grows
// past the size gate, HasPrefix fails so the object is re-read whole, and the
// local journal copy no longer contains the op the victim already applied.
//
// The next materialize folds a state the file is not in, and it is removed
// from the working folder. No delete op names it, nothing appears in History,
// and the peer chose all of it by rewriting one line of an object it owns —
// exactly the harm round 6 wrote the guard for.
func TestSec_Pull_AnInertOpCannotBuyAPeerTheRightToUndoAnAppliedOp(t *testing.T) {
	be := sharedRemote(t)
	d := newDevice(t, "victim", be)

	b1 := secfx5Blob(t, be, "first")
	b2 := secfx5Blob(t, be, "the content of a file the peer later un-publishes")

	l1 := secfx5Line(t, secfx5Op(1, "bob", "a.txt", b1, 5))
	l2 := secfx5Line(t, secfx5Op(2, "bob", "notes.md", b2, 48))

	secfx5PublishJournal(t, be, "bob", append(append([]byte{}, l1...), l2...))
	cycle(t, d)
	if !secfx5Exists(d.Folder, "notes.md") {
		t.Fatalf("control failed: notes.md never landed, so there is nothing to un-publish")
	}
	if !secfx5Exists(d.Folder, "a.txt") {
		t.Fatalf("control failed: a.txt never landed")
	}

	// Same op1; op2's line replaced by a LONGER line that still decodes to
	// exactly one op. The padding lives in the path of a delete for a file
	// that has never existed on any device.
	pad := secfx5Line(t, secfx6Del(2, "bob", "never-existed-"+strings.Repeat("p", len(l2)+64)+".txt"))
	if len(pad) <= len(l2) {
		t.Fatalf("fixture is wrong: the rewritten object must grow past the size gate")
	}
	rewritten := append(append([]byte{}, l1...), pad...)

	// The guard's own arithmetic, spelled out, so a failure below is not about
	// the fixture: the object still parses to as many ops as we had accepted.
	prev, _ := journal.Parse(append(append([]byte{}, l1...), l2...))
	all, _ := journal.Parse(rewritten)
	if len(all) < len(prev) {
		t.Fatalf("fixture is wrong: len(all)=%d < len(prev)=%d, so the count guard stops it "+
			"for the reason round 6 intended", len(all), len(prev))
	}

	secfx5PublishJournal(t, be, "bob", rewritten)
	cycle(t, d)

	if !secfx5Exists(d.Folder, "a.txt") {
		t.Errorf("control: a.txt vanished too — the rewrite dropped more than the targeted op")
	}
	if !secfx5Exists(d.Folder, "notes.md") {
		t.Errorf("a peer un-published an op every device had already applied and notes.md was "+
			"removed from the victim's working folder. No delete op names it, nothing is in "+
			"History. Round 6's guard is `len(all) < len(prev)` — an op COUNT — so one inert "+
			"delete of a path that never existed pays for the removal of a real put.")
	}
	// And it stays gone: the local copy is now the rewritten object.
	cycle(t, d)
	if !secfx5Exists(d.Folder, "notes.md") {
		t.Errorf("notes.md is still missing after a further cycle: the undo is permanent")
	}
}

// TestSec_Pull_AnOpAppliedFromATornTailCannotBeSilentlyUnpublished is the
// other half of round 6's line-boundary fix, and it needs no padding at all.
//
// Round 6 trims the resume point back to the last newline:
//
//	accepted := local
//	if i := bytes.LastIndexByte(accepted, '\n'); i >= 0 { accepted = accepted[:i+1] }
//
// but the ops the device APPLIED came from journal.Parse of the WHOLE local
// bytes, and Parse does not require a trailing newline — bytes.Split leaves
// the last fragment as a line, and a complete JSON object with no "\n" after
// it decodes perfectly. So a peer publishes
//
//	stage 1: <op1>\n<op2>            (op2 complete, deliberately unterminated)
//
// and the device applies BOTH ops while `accepted` covers only op1. Now
// stage 2 replaces op2's line with anything at all and still HasPrefix
// `<op1>\n` — so the resume takes the append branch, the "must not shrink the
// op count" guard is never consulted (it lives in the OTHER switch arm), the
// local copy is overwritten, and op2 is undone.
//
// The file disappears from the working folder with no delete op, nothing in
// the journal and nothing in History, and the peer chose it by leaving one
// newline off a publish.
func TestSec_Pull_AnOpAppliedFromATornTailCannotBeSilentlyUnpublished(t *testing.T) {
	be := sharedRemote(t)
	d := newDevice(t, "victim", be)

	b1 := secfx5Blob(t, be, "first")
	b2 := secfx5Blob(t, be, "the content of a file the peer later un-publishes")

	l1 := secfx5Line(t, secfx5Op(1, "bob", "a.txt", b1, 5))
	l2 := secfx5Line(t, secfx5Op(2, "bob", "notes.md", b2, 48))

	// op2's line, complete JSON, no trailing newline. An honest publisher
	// would never write this; a peer owns the object and writes what it likes.
	stage1 := append(append([]byte{}, l1...), l2[:len(l2)-1]...)
	secfx5PublishJournal(t, be, "bob", stage1)
	cycle(t, d)
	if !secfx5Exists(d.Folder, "notes.md") {
		t.Fatalf("control failed: op2 was not applied from the unterminated line, so there is " +
			"nothing to un-publish (Parse no longer accepts a line with no newline after it)")
	}

	// op1 untouched, op2's line replaced. This still extends `accepted`
	// (= "<op1>\n"), so it is treated as an ordinary append.
	pad := secfx5Line(t, secfx6Del(2, "bob", "never-existed-"+strings.Repeat("p", len(l2)+64)+".txt"))
	stage2 := append(append([]byte{}, l1...), pad...)
	if len(stage2) <= len(stage1) {
		t.Fatalf("fixture is wrong: the object must grow past the size gate")
	}
	secfx5PublishJournal(t, be, "bob", stage2)
	cycle(t, d)

	if !secfx5Exists(d.Folder, "a.txt") {
		t.Errorf("control: a.txt vanished too — the rewrite dropped more than the targeted op")
	}
	if !secfx5Exists(d.Folder, "notes.md") {
		t.Errorf("an op the device had already APPLIED was un-published and notes.md was removed " +
			"from the working folder. `accepted` is trimmed to the last newline but the ops were " +
			"parsed from the untrimmed bytes, so op2 was applied and is outside the prefix the " +
			"resume protects — and because the rewritten object still HasPrefix that trimmed " +
			"prefix, round 6's op-count guard is never reached at all.")
	}
	cycle(t, d)
	if !secfx5Exists(d.Folder, "notes.md") {
		t.Errorf("notes.md is still missing after a further cycle: the undo is permanent")
	}
}
