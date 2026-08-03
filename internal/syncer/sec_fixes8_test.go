package syncer

// Round 9 — the target is round 8's own fixes, and specifically the most
// invasive one: Cycle step 2b, the pull-side RE-ASSERTION.
//
// When a peer republishes its journal without an op this device already
// applied, the device now restates that op in its OWN journal, keeping the
// original lamport/time. That is a device writing an op it did not author,
// under its own identity, on a trigger a peer controls completely. The CISO's
// own gap list asks the two questions these tests answer:
//
//   - can a peer make a victim author ops it never made?
//   - is a re-assertion bounded to what this device actually holds?
//
// Helpers are prefixed secfx8; no existing file is touched.

import (
	"context"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// secfx8MyOps reads the ops this device published in its OWN journal — the one
// journal it is allowed to write, and the one every teammate and the hub's
// History attribute to it.
func secfx8MyOps(t *testing.T, s *Session) []journal.Op {
	t.Helper()
	ops, err := journal.ReadFile(s.Store.JournalPath(s.Device.ID))
	if err != nil {
		t.Fatalf("read own journal: %v", err)
	}
	return ops
}

func secfx8Paths(ops []journal.Op) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Kind+" "+op.Path)
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. Laundering: a peer makes the victim author a file nobody ever published.
// ---------------------------------------------------------------------------

// The victim makes NO local edit at all. A peer publishes content, lets the
// victim apply it, then republishes its own journal object with that line
// replaced and a different version of the same path appended.
//
// Step 2b re-asserts the withdrawn op into the victim's journal at its
// ORIGINAL (low) lamport — so it is, by construction, a losing local unpushed
// op the moment it is written. Step 3 (conflictCopies) then does exactly what
// it exists to do for a real local edit: it preserves the loser as a conflict
// copy, at a NEW path, in the victim's working folder, journaled under the
// victim's device and account and pushed to the hub.
//
// The net effect is that a peer alone — with no local edit on the victim's
// side to collide with — makes the victim create, sign and publish a file
// holding content the PEER chose, at a path that never existed in the project.
// The peer's own journal carries only the benign version.
//
// The control below is the same peer publishing a second version the honest
// way (a pure append): no conflict copy, no op authored by the victim. The
// delta is the finding.
func TestSec_Reassert_APeerCannotMakeAVictimAuthorAFileNobodyPublished(t *testing.T) {
	// ---- control: an honest append by the same peer ----
	ctlStorage := sharedRemote(t)
	ctl := newDevice(t, "victim", ctlStorage)
	ctlPoison := secfx4Blob(t, ctlStorage, "POISONED AGENT INSTRUCTIONS")
	ctlBenign := secfx4Blob(t, ctlStorage, "harmless project notes")
	ctlV1 := secfx7Op(t, "mal", 1, journal.KindPut, "CLAUDE.md", ctlPoison, 1) + "\n"
	secfx4Journal(t, ctlStorage, "mal", ctlV1)
	cycle(t, ctl)
	secfx4Journal(t, ctlStorage, "mal",
		ctlV1+secfx7Op(t, "mal", 2, journal.KindPut, "CLAUDE.md", ctlBenign, 1)+"\n")
	cycle(t, ctl)
	if ops := secfx8MyOps(t, ctl); len(ops) != 0 {
		t.Fatalf("control is broken: an honest append by a peer already made the victim author %v — "+
			"the attack below would then be measuring nothing", secfx8Paths(ops))
	}

	// ---- attack: the same two versions, published as a withdrawal ----
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", storage)

	poison := secfx4Blob(t, storage, "POISONED AGENT INSTRUCTIONS")
	benign := secfx4Blob(t, storage, "harmless project notes")

	op1 := secfx7Op(t, "mal", 1, journal.KindPut, "CLAUDE.md", poison, 1) + "\n"
	secfx4Journal(t, storage, "mal", op1)
	cycle(t, victim)
	if got := secfx4Tree(t, victim.Folder); got["CLAUDE.md"] == "" {
		t.Fatalf("control: the victim never applied CLAUDE.md at all, tree=%v", secfx4Keys(got))
	}
	before := secfx4Tree(t, victim.Folder)

	// The rewrite: seq 1's line is replaced by bytes journal.Parse drops, and
	// one op is appended so the op count does not fall. Byte for byte this is
	// what round 8's own reproducer publishes.
	junk := "{not json at all\n"
	op2 := secfx7Op(t, "mal", 2, journal.KindPut, "CLAUDE.md", benign, 1) + "\n"
	secfx4Journal(t, storage, "mal", junk+op2)

	cycle(t, victim)

	mine := secfx8MyOps(t, victim)
	after := secfx4Tree(t, victim.Folder)
	for _, op := range mine {
		if _, existed := before[op.Path]; existed {
			continue // re-asserting a path this device really held is the fix
		}
		t.Errorf("the victim published %s %q — a path that has never existed in this project.\n"+
			"  content of it:      %q\n"+
			"  note on the op:     %q\n"+
			"  victim's folder:    %v\n"+
			"  ops the victim now claims to have authored: %v\n"+
			"The victim made NO local edit. A peer republished its own journal with one applied "+
			"line withdrawn, step 2b re-asserted that op at its original (losing) lamport, and "+
			"conflictCopies then preserved it as a NEW file — content the PEER chose, created in "+
			"the victim's working folder, signed by the victim's device and account and pushed to "+
			"the hub, while the peer's journal shows only the benign version. The identical pair "+
			"of versions published as a plain append (the control above) makes the victim author "+
			"nothing.", op.Kind, op.Path, after[op.Path], op.Note, secfx4Keys(after), secfx8Paths(mine))
	}
}

// ---------------------------------------------------------------------------
// 2. Re-assertion is not bounded to what this device actually holds.
// ---------------------------------------------------------------------------

// Step 2b's premise, in its own words: "An op this device already applied is a
// file on this disk, and a file only leaves a disk because somebody deleted
// it." That premise is checked for a PUT (the store must hold the blob) and
// not at all for a DELETE — and `applied` is every op journal.Parse finds in
// the local copy of the peer's journal, not the ops that changed anything.
//
// So a peer pads its journal with ops that are valid and completely inert —
// deletes of paths the project has never contained, which is round 7's own
// padding primitive — and then withdraws them. The victim re-asserts every one
// of them into its own journal, as its own op, and pushes it: ops it did not
// author, for files that do not exist, that nothing on this disk stands behind.
//
// It is also the loop the CISO asked to be verified. A withdrawn SLOT is
// re-asserted once, but nothing stops a peer from minting fresh slots forever:
// each rewrite costs the peer one journal PUT and costs every teammate a
// permanent block of ops in their own append-only log.
func TestSec_Reassert_AWithdrawnOpForAPathThisDeviceNeverHeldIsNotReasserted(t *testing.T) {
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", storage)

	// v1: three deletes of paths that have never existed anywhere in the
	// project. Applying them changes nothing on any disk.
	var v1 strings.Builder
	for i := 1; i <= 3; i++ {
		v1.WriteString(secfx7Op(t, "mal", int64(i), journal.KindDelete,
			"ghost-"+string(rune('a'+i))+".md", "", 0) + "\n")
	}
	secfx4Journal(t, storage, "mal", v1.String())
	cycle(t, victim)

	if ops := secfx8MyOps(t, victim); len(ops) != 0 {
		t.Fatalf("control: the victim authored %v just from applying the peer's first journal", secfx8Paths(ops))
	}
	tree := secfx4Tree(t, victim.Folder)
	if len(tree) != 0 {
		t.Fatalf("control: the inert ops were not inert — the folder holds %v", secfx4Keys(tree))
	}

	// v2: all three lines replaced by bytes Parse drops, three fresh inert
	// ops appended so the op count does not fall. Every slot the victim
	// applied is now simply GONE.
	var v2 strings.Builder
	for i := 0; i < 3; i++ {
		v2.WriteString("{not json at all\n")
	}
	for i := 4; i <= 6; i++ {
		v2.WriteString(secfx7Op(t, "mal", int64(i), journal.KindDelete,
			"ghost-"+string(rune('a'+i))+".md", "", 0) + "\n")
	}
	secfx4Journal(t, storage, "mal", v2.String())
	cycle(t, victim)

	mine := secfx8MyOps(t, victim)
	if len(mine) == 0 {
		return
	}
	t.Errorf("the victim's own journal now carries %d ops it never authored: %v\n"+
		"  note on them: %q\n"+
		"None of these paths has ever existed in this project and the victim's folder is empty, so "+
		"there is no file on this disk for any of them to stand behind. Step 2b applies its "+
		"\"content we actually hold\" guard to puts only, and re-asserts every withdrawn DELETE "+
		"unconditionally — including ops that were inert when they were applied. A peer mints "+
		"fresh slots and withdraws them in a loop for one journal PUT each, and every teammate's "+
		"append-only journal grows by that many ops, permanently, for content that does not exist.",
		len(mine), secfx8Paths(mine), mine[0].Note)
}

// ---------------------------------------------------------------------------
// 3. Re-assertion ignores this device's own path filter.
// ---------------------------------------------------------------------------

// The ignore filter is applied symmetrically in scan and materialize: a path
// the rules exclude is neither written to this folder nor published from it.
// Step 2b is a third place a device publishes ops and it consults neither the
// filter nor neverSync — it takes the op straight off the peer's withdrawn
// list and appends it to this device's own journal.
//
// So a path this device deliberately refuses to materialize is re-published
// BY this device, under its own identity, at the first rewrite a peer makes.
func TestSec_Reassert_AnIgnoredPathIsNotRepublishedByThisDevice(t *testing.T) {
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", storage)

	write(t, victim.Folder, IgnoreFile, "secret/\n")

	key := secfx4Blob(t, storage, "-----BEGIN PRIVATE KEY-----")
	other := secfx4Blob(t, storage, "ordinary note")
	op1 := secfx7Op(t, "mal", 1, journal.KindPut, "secret/key.pem", key, 1) + "\n"
	secfx4Journal(t, storage, "mal", op1)
	cycle(t, victim)

	if tree := secfx4Tree(t, victim.Folder); tree["secret/key.pem"] != "" {
		t.Fatalf("control: the ignore rule did not hold on materialize; tree=%v", secfx4Keys(tree))
	}
	filter, err := loadFilter(victim.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !filter.Skip("secret/key.pem") {
		t.Fatalf("control: this device's own filter does not skip secret/key.pem")
	}

	secfx4Journal(t, storage, "mal",
		"{not json at all\n"+secfx7Op(t, "mal", 2, journal.KindPut, "notes.md", other, 1)+"\n")
	cycle(t, victim)

	for _, op := range secfx8MyOps(t, victim) {
		if !filter.Skip(op.Path) {
			continue
		}
		t.Errorf("this device published %s %q, a path its own %s excludes (note: %q).\n"+
			"The filter is applied symmetrically in scan and materialize precisely so a path this "+
			"device refuses to write is also a path it never publishes. Step 2b is a third "+
			"publishing site and it consults neither the filter nor neverSync: one journal rewrite "+
			"by any peer makes this device the publisher of every excluded path it happens to hold "+
			"a blob for.", op.Kind, op.Path, IgnoreFile, op.Note)
	}
}

// ---------------------------------------------------------------------------
// 4. sizeBound's bound on a blob is the number the PEER declared.
// ---------------------------------------------------------------------------

// secfx8Cycle runs a cycle without failing on Offline: a degraded pull is
// exactly what this attack produces, and the package's own posture is that a
// degraded cycle retries next time.
func secfx8Cycle(t *testing.T, s *Session) *Result {
	t.Helper()
	res, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	return res
}

// Round 8 bounded the two unbounded reads on the device side. The journal body
// is bounded by the size the LIST reported, which the storage owns. The blob
// body is bounded by `op.Size` — a field of the op, i.e. a number the PEER
// wrote into its own journal, with no relation to the object it names.
//
// Understate it and io.LimitReader truncates an honest blob mid-stream. The
// sha then does not match, and that is not a skip: `pull` RETURNS the error,
// so every blob queued behind the lie in the same batch is never fetched. The
// journal has already been written to disk by then, so the next cycle sees the
// peer's object unchanged, produces no new ops, and never re-enters the loop —
// those files are missing on this device permanently, with no error the user
// ever sees again after the first cycle.
//
// So one integer in one line of a journal any project member may push decides
// which files each teammate does not receive. That is the divergence primitive
// the byte-offset resume exists to prevent, arriving through the fix for the
// unbounded read.
func TestSec_Bounds_AnUnderstatedOpSizeCannotWithholdAPeersOtherFiles(t *testing.T) {
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", storage)

	big := secfx4Blob(t, storage, strings.Repeat("x", 3<<20)) // > the 1 MiB slack
	after := secfx4Blob(t, storage, "the file queued behind the lie")

	// op1 names a real 3 MiB blob and DECLARES it as 1 byte. op2 is entirely
	// honest and is simply behind it in the batch.
	body := secfx7Op(t, "mal", 1, journal.KindPut, "big.md", big, 1) + "\n" +
		secfx7Op(t, "mal", 2, journal.KindPut, "after.md", after, 31) + "\n"
	secfx4Journal(t, storage, "mal", body)

	for i := 0; i < 3; i++ {
		secfx8Cycle(t, victim)
	}

	tree := secfx4Tree(t, victim.Folder)
	if tree["after.md"] != "" {
		return // the honest file arrived: the secure outcome
	}
	t.Errorf("after.md never reached this device, and never will.\n"+
		"  victim's folder after 3 cycles: %v\n"+
		"  after.md's own op is completely honest; the op BEFORE it declared Size 1 for a 3 MiB blob.\n"+
		"sizeBound bounds the blob GET by op.Size — a peer-written field with no relation to the "+
		"object it names. The truncated read fails the sha check, `pull` returns the error instead "+
		"of skipping the one blob, and everything queued behind it in the batch is dropped. The "+
		"peer's journal is already written to local disk, so the next cycle produces no new ops "+
		"and the loop is never re-entered: a peer chooses, per teammate, which files simply do not "+
		"arrive — silently, after the first cycle.", secfx4Keys(tree))
}
