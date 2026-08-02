package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Round 6 — attacking round 5's own fix for the pull cursor (row 15).
//
// Round 5 replaced pull's op-COUNT cursor with a BYTE offset:
//
//	if bytes.Equal(local, data) { continue }
//	tail := data
//	if len(local) > 0 && bytes.HasPrefix(data, local) { tail = data[len(local):] }
//	fresh, _ := journal.Parse(tail)
//
// The offset is the length of whatever bytes we last stored, and nothing
// requires those bytes to end on a line boundary.

// secfx5Blob stores content in the remote under its content address and
// returns the sha, the way push does.
func secfx5Blob(t *testing.T, be remote.Backend, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	key := hex.EncodeToString(sum[:])
	if err := be.Put(context.Background(), "blobs/"+key, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	return key
}

// secfx5Op is one ordinary put op from a peer.
func secfx5Op(seq int64, dev, path, blob string, size int64) journal.Op {
	return journal.Op{
		Seq:     seq,
		Lamport: seq,
		Time:    time.Date(2026, 8, 1, 12, 0, int(seq), 0, time.UTC),
		Device:  dev,
		Kind:    journal.KindPut,
		Path:    path,
		Blob:    blob,
		Size:    size,
		Mode:    0o644,
	}
}

func secfx5Line(t *testing.T, op journal.Op) []byte {
	t.Helper()
	b, err := journal.Marshal([]journal.Op{op})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// secfx5PublishJournal writes a peer's journal object verbatim. A peer owns
// its own journal key, so every byte here is a byte a hostile member may put.
func secfx5PublishJournal(t *testing.T, be remote.Backend, dev string, data []byte) {
	t.Helper()
	if err := be.Put(context.Background(), "journal/"+dev+".jsonl", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

func secfx5Exists(folder, rel string) bool {
	_, err := os.Stat(filepath.Join(folder, filepath.FromSlash(rel)))
	return err == nil
}

// TestSec_Pull_ATornTailFromAPeerCannotHideAnOpForever is round 5's divergence
// primitive in new clothes.
//
// Bob publishes his journal in two stages, both append-only, both honestly
// sized, both growing:
//
//	stage 1: <op1>\n<the first half of op2's line, no newline>
//	stage 2: <op1>\n<op2>\n<op3>\n
//
// A device that pulls between the two stages stores stage 1's bytes verbatim,
// including the half line. At the next pull stage 2 still HasPrefix those
// bytes, so the resume offset lands in the MIDDLE of op2's JSON. The tail
// parses as `.md",...}` — undecodable, silently skipped by Parse — so that
// device never applies op2 and never will: the local copy is now the full
// object, so the next resume starts after it.
//
// A device that first pulls after stage 2 applies all three ops. One journal,
// two permanent states, and the peer picked the split by choosing where to cut
// stage 1 — which is precisely the property the byte offset was introduced to
// remove.
func TestSec_Pull_ATornTailFromAPeerCannotHideAnOpForever(t *testing.T) {
	be := sharedRemote(t)
	early := newDevice(t, "early", be)    // pulls between the two stages
	latecomer := newDevice(t, "late", be) // pulls only after stage 2

	b1 := secfx5Blob(t, be, "harmless first file")
	b2 := secfx5Blob(t, be, "the op the peer wants one device to miss")
	b3 := secfx5Blob(t, be, "harmless third file")

	l1 := secfx5Line(t, secfx5Op(1, "bob", "a.txt", b1, 19))
	l2 := secfx5Line(t, secfx5Op(2, "bob", "secret.md", b2, 40))
	l3 := secfx5Line(t, secfx5Op(3, "bob", "c.txt", b3, 19))

	// Stage 1: op1, then op2's line cut in half — no newline, nothing after it.
	torn := append(append([]byte{}, l1...), l2[:len(l2)/2]...)
	full := append(append(append([]byte{}, l1...), l2...), l3...)
	if !bytes.HasPrefix(full, torn) {
		t.Fatalf("fixture is wrong: stage 2 must extend stage 1")
	}

	secfx5PublishJournal(t, be, "bob", torn)
	cycle(t, early)
	if !secfx5Exists(early.Folder, "a.txt") {
		t.Fatalf("control failed: the early device did not even apply op1 from stage 1")
	}
	if secfx5Exists(early.Folder, "secret.md") {
		t.Fatalf("control failed: a half-written line materialized a file")
	}

	secfx5PublishJournal(t, be, "bob", full)
	cycle(t, early)
	cycle(t, latecomer)

	// Control: a device reading the same object for the first time gets it all.
	for _, rel := range []string{"a.txt", "secret.md", "c.txt"} {
		if !secfx5Exists(latecomer.Folder, rel) {
			t.Fatalf("control failed: the latecomer is missing %s, so the journal itself is bad", rel)
		}
	}
	// The finding: same journal, same bytes, different state — forever.
	if !secfx5Exists(early.Folder, "secret.md") {
		got, _ := os.ReadDir(early.Folder)
		var names []string
		for _, e := range got {
			names = append(names, e.Name())
		}
		t.Fatalf("the early device permanently lost op2: its folder holds %v while a device "+
			"reading the same journal object holds a.txt, secret.md and c.txt — "+
			"the peer chose the split by cutting stage 1 mid-line", names)
	}
	// And it stays lost: another cycle does not recover it.
	cycle(t, early)
	if !secfx5Exists(early.Folder, "secret.md") {
		t.Fatalf("op2 is still missing after a further cycle")
	}
}

// TestSec_Pull_APeerCannotDropAnAlreadyAppliedOpByRewritingItsJournal covers
// the other half of "an object that does not extend our bytes is re-read
// whole". Re-reading whole is only safe if the object still CONTAINS the ops
// we already applied. Round 5 removed the `len(fresh) <= len(prev)` guard that
// used to make a shrinking journal a no-op, and replaced it with a byte
// comparison that happily overwrites the local copy with a rewritten object,
// as long as the object grew.
//
// So a peer replaces the line of an op every device has already applied with a
// longer line that does not decode. The object grows (past the size gate), it
// no longer extends our bytes (so it is re-read whole), and the op is gone
// from the local journal copy — with nothing in any journal, and nothing in
// History, recording the removal.
func TestSec_Pull_APeerCannotDropAnAlreadyAppliedOpByRewritingItsJournal(t *testing.T) {
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

	// Same op1, op2's line replaced by a longer line that decodes to nothing.
	junk := []byte(strings.Repeat("x", len(l2)+64) + "\n")
	secfx5PublishJournal(t, be, "bob", append(append([]byte{}, l1...), junk...))
	cycle(t, d)

	if !secfx5Exists(d.Folder, "notes.md") {
		t.Fatalf("a peer removed an op it had already published and the file vanished from " +
			"the victim's folder — no delete op, nothing in the journal, nothing in History")
	}
}

// TestSec_SyncMeta_AFutureOpTimeCannotOutrankRealHistory is round 5's
// DisplayTime clamp, attacked on the field it left alone.
//
// Round 5 fixed `bdrive log` being ownable by a peer: DisplayTime preferred
// Op.Mtime, "a peer's unverified claim", so a year-9999 mtime sat above every
// real entry forever. The clamp it shipped is
//
//	if !op.Mtime.IsZero() && !op.Mtime.After(op.Time) { return op.Mtime }
//	return op.Time
//
// which bounds one peer-chosen value by another peer-chosen value. Op.Time is
// no more verified than Op.Mtime — it is a field in the same JSON line, off
// the same journal, written by the same peer — so the exact harm round 5
// named ("a handful of them pushes the genuine history off a screen that
// prints 50 rows") is reproduced by moving the value one field to the left.
func TestSec_SyncMeta_AFutureOpTimeCannotOutrankRealHistory(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	for _, rel := range []string{"a.md", "b.md", "c.md"} {
		write(t, victim.Folder, rel, "mine "+rel)
	}
	cycle(t, victim)

	const content = "peer content"
	blob := secfx5Blob(t, be, content)
	var data []byte
	for i := 1; i <= 3; i++ {
		op := secfx5Op(int64(i), "attacker", "peer/"+string(rune('a'+i-1))+".md", blob, int64(len(content)))
		// No mtime at all, so round 5's clamp never even engages.
		op.Mtime = time.Time{}
		op.Time = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		data = append(data, secfx5Line(t, op)...)
	}
	secfx5PublishJournal(t, be, "attacker", data)
	cycle(t, victim)

	entries, err := LogEntries(victim.Store, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	SortForDisplay(entries)
	if len(entries) < 6 {
		t.Fatalf("control failed: only %d log entries, want the victim's 3 plus the peer's 3", len(entries))
	}

	limit := time.Now().Add(time.Hour)
	for _, op := range entries {
		if shown := DisplayTime(op); shown.After(limit) {
			t.Errorf("%s displays as %v, in the future — Op.Time is a sort key the peer picked, "+
				"exactly like Op.Mtime was", op.Path, shown)
		}
	}
	top := entries[:3]
	all := true
	for _, op := range top {
		if !strings.HasPrefix(op.Path, "peer/") {
			all = false
		}
	}
	if all {
		t.Errorf("the newest 3 rows of `bdrive log` are all the peer's (%s, %s, %s) and stay there "+
			"forever; the operator's own changes are pushed below every one of them",
			top[0].Path, top[1].Path, top[2].Path)
	}
}
