package syncer

// Round 6 — auditing round 3's own headline client fix.
//
// TestSec_SyncJournal_PeerCannotMaterializeOutsideTheMount (round 3) and
// TestSec_Sync_PeerJournalCannotMaterializeReservedPaths (round 1) both stay
// GREEN when unsafeRel is made to return false for everything. They survive on
// round 4's store.UnderRoot check inside writeFile, which answers the same
// question about the filesystem rather than about the string.
//
// Defence in depth is right. A test that silently changes which layer it
// measures is not: the layer it stopped measuring can be deleted by a refactor
// with the whole suite green, and unsafeRel catches two things UnderRoot
// cannot, because UnderRoot only ever answers "does this land inside the
// root":
//
//   - an absolute Op.Path. filepath.Join(folder, "/etc/passwd") is
//     folder/etc/passwd — inside the root, so UnderRoot approves, and a peer
//     names a file the journal's own path never described.
//   - an unclean Op.Path. "docs/../notes.md" and "notes.md" are one file after
//     Join, so one journal holds two paths for it and last-writer-wins is
//     decided by which spelling sorted last rather than by Less.
//
// Helpers are prefixed secaud; no existing file is touched.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// secaudTree lists every entry under dir, so a cycle's effect outside the
// mount can be compared before and after.
func secaudTree(t *testing.T, dir string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			got[p] = true
		}
		return nil
	})
	return got
}

// unsafeRel is the guard neverSync consults for every path that reaches the
// working folder, on the write side and the delete side both. It judges the
// SPELLING, and it refuses rather than normalizes on purpose: normalizing
// would land two different journal paths on one file.
func TestSec_Audit_UnsafeRelRefusesEveryPathAJournalMayNotName(t *testing.T) {
	// Control: the ordinary paths a scan produces must all be accepted, so a
	// refusal below is about the shape and not about the guard being shut.
	for _, ok := range []string{
		"notes.md",
		"wiki/q3/plan.md",
		"a b/c d.md",
		".bdriveignore",
		"docs/....md",
		"..hidden.md",
		"x/..y/z.md",
	} {
		if unsafeRel(ok) {
			t.Fatalf("control: unsafeRel(%q) = true — a legitimate synced path is refused", ok)
		}
	}

	for _, bad := range []struct{ name, rel string }{
		{"empty", ""},
		{"bare parent", ".."},
		{"leading parent", "../secret.txt"},
		{"deep escape", "../../../../.ssh/authorized_keys"},
		{"absolute", "/etc/passwd"},
		{"absolute home", "/Users/victim/.ssh/authorized_keys"},
		{"embedded parent", "docs/../../secret.txt"},
		{"unclean same-dir", "./notes.md"},
		{"unclean parent-hop", "docs/../notes.md"},
		{"bare dot", "."},
		{"double slash", "docs//notes.md"},
		{"trailing slash", "docs/"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if !unsafeRel(bad.rel) {
				t.Errorf("unsafeRel(%q) = false: neverSync lets a peer's op with this path through "+
					"to materialize. UnderRoot inside writeFile catches the spellings that land "+
					"outside the mount, but not an absolute path (Join puts it back inside), not an "+
					"unclean one (two journal paths, one file), and not %q — which resolves to the "+
					"mount root itself. remote.Prefixed.safeKey refuses %q on the hub side for "+
					"exactly this reason (round 5); the syncer never got the same rule. LOW: the "+
					"companion test below shows %q is contained today, but only because hashFile "+
					"happens to fail on a directory before writeFile is ever reached",
					bad.rel, ".", ".", ".")
			}
		})
	}
}

// "." is the one spelling unsafeRel accepts that is not a path at all. It is
// Clean-stable, not empty, not "..", not absolute — so neverSync lets it
// through, materializeFile resolves it to the mount root itself, and writeFile
// asks UnderRoot about the root, which is trivially inside itself. What
// happens next is os.CreateTemp(filepath.Dir(abs)) — and filepath.Dir of the
// mount root is the mount's PARENT, outside the boundary the whole guard
// exists to hold.
//
// remote.Prefixed.safeKey already refuses "." for the same reason on the hub
// side (round 5, TestSec_Prefixed_ADotIsNotAKey); the syncer never got the
// same rule. A peer chooses this with one line of JSON and no victim action.
func TestSec_Audit_ADotPathWritesNothingOutsideTheMount(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)
	// Give the mount a parent of its own, so anything appearing beside it is
	// unambiguously a write that left the boundary — newDevice's default
	// parent also holds the volume store, which every cycle legitimately
	// writes to.
	base := t.TempDir()
	victim.Folder = filepath.Join(base, "mount")
	if err := os.MkdirAll(victim.Folder, 0o755); err != nil {
		t.Fatal(err)
	}
	const content = "content the peer chose"
	blob := secjrnBlob(t, be, content)

	secjrnPush(t, be, "attacker", []journal.Op{
		secjrnOp(1, ".", blob, len(content)),
		secjrnOp(2, "notes/ok.md", blob, len(content)), // control
	})
	if _, err := secpeerCycle(t, victim); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	// Control: the ordinary op in the same journal did land, so the cycle ran
	// and a clean result below is not an inert test.
	if got := read(t, victim.Folder, "notes/ok.md"); got != content {
		t.Fatalf("control: the well-formed op did not materialize: %q", got)
	}
	// The mount root must still be a directory.
	if fi, err := os.Stat(victim.Folder); err != nil || !fi.IsDir() {
		t.Errorf("the mount root is no longer a directory after an op with Path %q: %v", ".", err)
	}
	ents, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() == "mount" {
			continue
		}
		t.Errorf("an op with Path %q created %q beside the mount root, outside the boundary "+
			"writeFile's UnderRoot check exists to hold: materializeFile resolves %q to the "+
			"mount root itself, and os.CreateTemp(filepath.Dir(abs)) then writes into its PARENT",
			".", filepath.Join(base, e.Name()), ".")
	}
}
