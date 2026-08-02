package syncer

// Round 10 — the target is round 9's own fixes to round 8's fixes:
//
//   - stillHold, the re-assertion admission predicate, and the move of step 2b
//     to AFTER conflictCopies (now 3b)
//   - the sizeBound sha-mismatch error, now remembered and returned after the
//     batch instead of abandoning it
//
// Helpers are prefixed secfx9; no existing file is touched.

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// secfx9NoPush is a hub that serves reads and refuses writes — a device whose
// member has READ permission on the project, which is the steady state
// Result.ReadOnly exists to describe ("we keep pulling, local ops stay
// journaled and unpushed"). It is also what an ordinary device looks like for
// the rest of any cycle whose push failed.
type secfx9NoPush struct{ remote.Backend }

func (b secfx9NoPush) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return remote.ErrForbidden
}

// ---------------------------------------------------------------------------
// 1. The re-assertion laundering hole, on a device that cannot push.
// ---------------------------------------------------------------------------
//
// Round 9 fixed round 8's laundering by moving re-assertion AFTER
// conflictCopies, so a re-asserted op (which carries the withdrawn op's
// original, losing lamport) is no longer a losing local unpushed op in the
// same cycle it is created.
//
// The ordering only holds for one cycle. st.PushedOps is what conflictCopies
// measures "unpushed" against, and it only advances on a SUCCESSFUL push — so
// on a device whose push is refused (a read-only member: the documented steady
// state, where local ops "stay journaled and unpushed" forever) the re-asserted
// op is in the unpushed set on every later cycle. The peer then touches the
// same path once more and conflictCopies does exactly what round 8's hole did:
// it preserves the peer-chosen, withdrawn content as a NEW file, in the
// victim's working folder, journaled under the victim's own device and account.
//
// The control is the same peer publishing the same three versions as plain
// appends: no withdrawal, no op authored by the victim, no conflict copy.
func TestSec_Reassert_ADeviceThatCannotPushIsNotMadeToAuthorAFileNobodyPublished(t *testing.T) {
	// ---- control: three honest appends, same read-only device ----
	ctlStorage := sharedRemote(t)
	ctl := newDevice(t, "victim", secfx9NoPush{ctlStorage})
	cPoison := secfx4Blob(t, ctlStorage, "POISONED AGENT INSTRUCTIONS")
	cV2 := secfx4Blob(t, ctlStorage, "harmless project notes v2")
	cV3 := secfx4Blob(t, ctlStorage, "harmless project notes v3")
	line1 := secfx7Op(t, "mal", 1, journal.KindPut, "CLAUDE.md", cPoison, 1) + "\n"
	line2 := secfx7Op(t, "mal", 2, journal.KindPut, "CLAUDE.md", cV2, 1) + "\n"
	line3 := secfx7Op(t, "mal", 3, journal.KindPut, "CLAUDE.md", cV3, 1) + "\n"
	write(t, ctl.Folder, "mine.md", "the read-only member's own note")
	secfx4Journal(t, ctlStorage, "mal", line1)
	secfx9Cycle(t, ctl)
	secfx4Journal(t, ctlStorage, "mal", line1+line2)
	secfx9Cycle(t, ctl)
	secfx4Journal(t, ctlStorage, "mal", line1+line2+line3)
	secfx9Cycle(t, ctl)
	for _, op := range secfx8MyOps(t, ctl) {
		if op.Path != "mine.md" { // its own edit is the only thing it may author
			t.Fatalf("control is broken: honest appends already made the read-only victim author %v",
				secfx8Paths(secfx8MyOps(t, ctl)))
		}
	}

	// ---- attack: the same three versions, published as withdrawals ----
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", secfx9NoPush{storage})

	poison := secfx4Blob(t, storage, "POISONED AGENT INSTRUCTIONS")
	v2 := secfx4Blob(t, storage, "harmless project notes v2")
	v3 := secfx4Blob(t, storage, "harmless project notes v3")

	op1 := secfx7Op(t, "mal", 1, journal.KindPut, "CLAUDE.md", poison, 1) + "\n"
	secfx4Journal(t, storage, "mal", op1)
	// One ordinary local file, so there is something for the hub to refuse and
	// Result.ReadOnly is the state the device actually settles into.
	write(t, victim.Folder, "mine.md", "the read-only member's own note")
	res := secfx9Cycle(t, victim)
	if !res.ReadOnly {
		t.Fatalf("fixture: the hub was expected to refuse the push (ReadOnly), got %+v", res)
	}
	if got := secfx4Tree(t, victim.Folder); got["CLAUDE.md"] != "POISONED AGENT INSTRUCTIONS" {
		t.Fatalf("fixture: the victim never applied CLAUDE.md, tree=%v", secfx4Keys(got))
	}
	before := secfx4Tree(t, victim.Folder)

	// Withdrawal #1: seq 1's line becomes bytes Parse drops; a second version
	// is appended so the op count does not fall. The victim re-asserts the
	// withdrawn op into its own journal — and cannot push it.
	junk := "{not json at all\n"
	op2 := secfx7Op(t, "mal", 2, journal.KindPut, "CLAUDE.md", v2, 1) + "\n"
	secfx4Journal(t, storage, "mal", junk+op2)
	secfx9Cycle(t, victim)

	// Withdrawal #2: the peer touches the same path once more. Nothing new is
	// required of it — any later op for this path will do.
	op3 := secfx7Op(t, "mal", 3, journal.KindPut, "CLAUDE.md", v3, 1) + "\n"
	secfx4Journal(t, storage, "mal", junk+junk+op3)
	secfx9Cycle(t, victim)

	after := secfx4Tree(t, victim.Folder)
	for _, op := range secfx8MyOps(t, victim) {
		if _, existed := before[op.Path]; existed {
			continue // restating a path this device really held is the fix
		}
		t.Errorf("a device that cannot push published %s %q — a path that has never existed in this project.\n"+
			"  content of it:   %q\n"+
			"  note on the op:  %q\n"+
			"  victim's folder: %v\n"+
			"The victim made NO local edit and holds READ permission only, so st.PushedOps never "+
			"advances and every re-asserted op stays in conflictCopies' unpushed set forever. "+
			"Round 9's ordering fix (re-assert after conflictCopies) protects only the cycle the "+
			"op is created in. The identical three versions published as plain appends (the "+
			"control above) make the victim author nothing.",
			op.Kind, op.Path, after[op.Path], op.Note, secfx4Keys(after))
	}
}

// ---------------------------------------------------------------------------
// 2. One integer in one peer journal line stops the victim's OWN ops leaving.
// ---------------------------------------------------------------------------
//
// Round 9 stopped a sha mismatch from abandoning the blob batch, so the files
// queued behind it now arrive. The error is still RETURNED, and Cycle turns
// any pull error into Result.Offline — which is the condition step 5 checks
// before pushing anything. So a peer that understates Op.Size on one line of
// its own journal makes the victim's cycle end offline, and the victim's own
// journal and blobs stay on the victim's disk.
//
// Op.Size is a field the peer writes with no relation to the object it names,
// and it costs one journal PUT per victim cycle to repeat. The victim is told
// only "offline".
//
// The control is the same content published with an honest size: the victim's
// work reaches the hub in the same cycle.
func TestSec_Pull_APeersBadBlobSizeDoesNotWithholdTheVictimsOwnPush(t *testing.T) {
	big := strings.Repeat("x", 2<<20) // must exceed the declared size + sizeGrowth

	// ---- control: the same blob, declared honestly ----
	ctlStorage := sharedRemote(t)
	ctl := newDevice(t, "victim", ctlStorage)
	cBlob := secfx4Blob(t, ctlStorage, big)
	secfx4Journal(t, ctlStorage, "mal",
		secfx7Op(t, "mal", 1, journal.KindPut, "peer.bin", cBlob, len(big))+"\n")
	write(t, ctl.Folder, "my-work.md", "the victim's own unsaved work")
	res := secfx9Cycle(t, ctl)
	if res.Offline || !res.Pushed {
		t.Fatalf("control is broken: an honest peer line already blocked the push (%+v, err=%v)",
			res, res.OfflineErr)
	}

	// ---- attack: the same blob, one integer changed ----
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", storage)
	blob := secfx4Blob(t, storage, big)
	secfx4Journal(t, storage, "mal",
		secfx7Op(t, "mal", 1, journal.KindPut, "peer.bin", blob, 1)+"\n")

	write(t, victim.Folder, "my-work.md", "the victim's own unsaved work")
	res = secfx9Cycle(t, victim)
	if res.LocalOps == 0 {
		t.Fatalf("fixture: the victim journaled nothing of its own")
	}
	if !res.Pushed {
		t.Errorf("the victim's own ops did not leave this device: Pushed=%v Offline=%v err=%v.\n"+
			"One peer journal line understating Op.Size for a blob the hub is serving honestly "+
			"truncates the read, fails the sha, and returns an error from pull — which Cycle "+
			"turns into Result.Offline, and step 5 pushes nothing when the cycle is offline. "+
			"The victim's own journal and blobs stay on the victim's disk, and the only thing "+
			"reported is \"offline\". The same content with an honest size (the control) pushes "+
			"in the same cycle.", res.Pushed, res.Offline, res.OfflineErr)
	}
	// And the hub genuinely has none of it.
	objs, err := storage.List(context.Background(), "journal/victim")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) == 0 {
		t.Errorf("the hub holds no journal for the victim at all after a cycle that committed %d local op(s)",
			res.LocalOps)
	}
}

// secfx9Cycle runs a cycle and tolerates ReadOnly (which the shared `cycle`
// helper does too) but still fails on a genuine error.
func secfx9Cycle(t *testing.T, s *Session) *Result {
	t.Helper()
	res, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}
