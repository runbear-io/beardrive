package syncer

// Round 5 — the target is round 4's own fixes (b616c94) in the sync engine:
// journal.Parse's skip-the-bad-line (whose cursor arithmetic lives HERE, in
// pull), the Filter.nested carry across the mid-cycle ignore-file reload, and
// store.UnderRoot, the new on-disk containment guard writeFile calls.
//
// Every test asserts the SECURE behavior, so it goes green the moment the hole
// is closed and stays as a permanent regression test. Helpers are prefixed
// secfx4; no existing file is touched.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

// ---- helpers -------------------------------------------------------------

// secfx4Blob puts content into the shared remote the way push does and returns
// its sha256, so a hand-written journal line can reference real content.
func secfx4Blob(t *testing.T, be remote.Backend, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	if err := be.Put(context.Background(), "blobs/"+sha, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	return sha
}

// secfx4Journal writes raw bytes to a device's journal key. A hostile peer
// owns its own journal object and nothing on the hub makes it append-only, so
// replacing it wholesale is inside the threat model.
func secfx4Journal(t *testing.T, be remote.Backend, dev, body string) {
	t.Helper()
	if err := be.Put(context.Background(), "journal/"+dev+".jsonl", strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
}

func secfx4Op(t *testing.T, seq int64, kind, path, blob string, size int) string {
	t.Helper()
	b, err := json.Marshal(journal.Op{
		Seq: seq, Lamport: seq, Time: time.Unix(1767225600+seq, 0).UTC(),
		Device: "mal", DeviceName: "mal", Kind: kind, Path: path, Blob: blob, Size: int64(size),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// secfx4Tree lists the mount's synced files with their contents.
func secfx4Tree(t *testing.T, folder string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(folder, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(folder, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".bdrive/") || strings.Contains(rel, "/.bdrive/") {
			return nil
		}
		b, _ := os.ReadFile(p)
		out[rel] = string(b)
		return nil
	})
	return out
}

func secfx4Keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// pull's cursor is len(prev) — an op count. Parse now drops lines. A peer
// controls both.
// ---------------------------------------------------------------------------

// Round 4 made journal.Parse skip a line it cannot decode, on the grounds that
// "every reader drops the same lines from the same bytes, so replay stays in
// agreement". That is true of the BYTES. It is not true of pull, which does
// not diff bytes — it diffs COUNTS:
//
//	prev, _ := s.Store.DeviceOps(dev)      // parse of the copy we already had
//	if len(fresh) <= len(prev) { continue }
//	newOps = append(newOps, fresh[len(prev):]...)
//
// So the number of ops a peer's journal parses to is the index every other
// device resumes from. A peer that replaces its own journal object (nothing
// makes it append-only — /store/object PUT overwrites) can insert one
// undecodable line among the lines a device has ALREADY counted, and every op
// it appends shifts down by one. A device that pulled earlier skips exactly
// that many of the peer's new ops; a device that pulls for the first time
// applies all of them. Both then advance their cursor past the gap, so it is
// permanent.
//
// The result is the invariant CLAUDE.md guards hardest: two devices replaying
// one journal converge to different states, and the peer chooses which ops
// each of them loses. Dropping a `delete` on one device and keeping it on
// another is the same primitive.
func TestSec_Pull_APeerCannotChooseWhichOpsEachDeviceSees(t *testing.T) {
	storage := sharedRemote(t)
	early := newDevice(t, "early", storage)
	late := newDevice(t, "late", storage)

	blobA := secfx4Blob(t, storage, "A")
	blobB := secfx4Blob(t, storage, "B")
	blobC := secfx4Blob(t, storage, "C")
	blobD := secfx4Blob(t, storage, "D")

	op1 := secfx4Op(t, 1, journal.KindPut, "a.md", blobA, 1)
	op2 := secfx4Op(t, 2, journal.KindPut, "b.md", blobB, 1)
	op3 := secfx4Op(t, 3, journal.KindPut, "c.md", blobC, 1)

	// v1: three ordinary ops. "early" syncs and counts three.
	secfx4Journal(t, storage, "mal", op1+op2+op3)
	cycle(t, early)
	if got := secfx4Tree(t, early.Folder); len(got) != 3 {
		t.Fatalf("setup: early has %v, want a.md b.md c.md", secfx4Keys(got))
	}

	// v2: one of the counted lines is replaced by junk, and two new ops are
	// appended. Parse yields 4 ops, so early resumes at index 3 and never
	// sees the FIRST of the two new ops.
	junk := "{" + strings.Repeat("#", 400) + "\n" // undecodable, and grows the file
	op4 := secfx4Op(t, 4, journal.KindPut, "d.md", blobD, 1)
	op5 := secfx4Op(t, 5, journal.KindDelete, "a.md", "", 0)
	secfx4Journal(t, storage, "mal", op1+junk+op3+op4+op5)

	cycle(t, early)
	cycle(t, late) // first sync ever: it parses the same 4 ops from index 0

	gotEarly, gotLate := secfx4Tree(t, early.Folder), secfx4Tree(t, late.Folder)
	if !equalTrees(gotEarly, gotLate) {
		t.Errorf("two devices replaying ONE journal hold different states:\n"+
			"  device that synced before the rewrite: %v\n"+
			"  device syncing for the first time:     %v\n"+
			"pull resumes at len(prev) — an op COUNT — and Parse now silently drops lines, "+
			"so the peer that wrote the journal decides how far each device's cursor jumps",
			secfx4Keys(gotEarly), secfx4Keys(gotLate))
	}
}

func equalTrees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The nested-mount boundary across the mid-cycle ignore-file reload.
// ---------------------------------------------------------------------------

// Round 4 noticed that reloading the filter after materializing a pulled
// .bdriveignore threw away Filter.nested, and carried the old value across:
//
//	nested := filter.nested
//	filter, err = loadFilter(...)
//	filter.nested = nested
//
// But nested is not a property of the rules — it is what walkFolder DISCOVERED
// under the rules in force during this cycle's scan, and walkFolder checks
// PruneDir BEFORE config.IsMount:
//
//	case ignoredDir(d.Name()) || filter.PruneDir(rel): v = vPruneDir
//	case config.IsMount(p):                            filter.addNestedMount(rel)
//
// So a nested mount inside a directory the OLD rules pruned is never
// discovered, and the carried-over list is empty for exactly that directory.
// A peer that pushes a .bdriveignore un-ignoring it, in the same journal push
// as an op under it, gets this project to write into a folder that syncs
// through a DIFFERENT project — whose daemon then pushes the file on to that
// project's members. One op crosses a project boundary in both directions.
func TestSec_Cycle_ReloadedRulesCannotWriteIntoANestedMount(t *testing.T) {
	storage := sharedRemote(t)
	victim := newDevice(t, "victim", storage)
	peer := newDevice(t, "peer", storage)

	// The victim's folder: vendor/ is ignored, and vendor/shared is a mount of
	// its own (its own project, its own members).
	write(t, victim.Folder, IgnoreFile, "vendor/\n")
	write(t, victim.Folder, "vendor/shared/.bdrive/config.json", `{"mount_id":"other","volume":"other"}`)
	write(t, victim.Folder, "vendor/shared/own.md", "belongs to the other project")
	cycle(t, victim)

	// The peer sends both halves in one push: rules that un-ignore vendor/,
	// and an op for a path inside the nested mount.
	cycle(t, peer)
	write(t, peer.Folder, IgnoreFile, "# nothing is ignored\n")
	write(t, peer.Folder, "vendor/shared/planted.md", "written by this project")
	cycle(t, peer)

	cycle(t, victim)

	planted := filepath.Join(victim.Folder, "vendor", "shared", "planted.md")
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("this project materialized a file inside a NESTED mount (%s):\n"+
			"the nested list carried across the reload was built by a scan that never "+
			"descended into vendor/ (PruneDir wins over config.IsMount in walkFolder), "+
			"so the pulled rules re-opened a project boundary the carry was meant to hold", planted)
	}
	if got := read(t, victim.Folder, "vendor/shared/own.md"); got != "belongs to the other project" {
		t.Errorf("the nested mount's own file changed: %q", got)
	}
}

// ---------------------------------------------------------------------------
// store.UnderRoot — the guard writeFile consults before MkdirAll.
// ---------------------------------------------------------------------------

// UnderRoot resolves the deepest EXISTING ancestor of p and asks whether that
// is inside root. A symlink whose target does not exist yet is not an existing
// ancestor, so it is walked straight past: EvalSymlinks fails on it, the loop
// steps up to its parent, the parent is inside root, and the answer is "yes"
// for a path that resolves outside root the moment anything creates the
// target. The two shapes below both answer yes today.
//
// Nothing in the current syncer escapes through them — writeFile's temp+rename
// happens to replace a symlink rather than follow it, and MkdirAll refuses a
// dangling one — but that is an accident of two callers' write styles, not
// something UnderRoot promises, and it is the single containment primitive
// remote/local.go now leans on for the hub's storage root as well. A guard
// whose answer is "inside" for a path that lands outside is a guard that will
// be wrong the first time a caller uses os.OpenFile.
func TestSec_UnderRoot_ADanglingSymlinkIsNotInsideTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "mount")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// final component is a symlink to a not-yet-existing file outside root
	if err := os.Symlink(filepath.Join(outside, "authorized_keys"), filepath.Join(root, "notes.md")); err != nil {
		t.Fatal(err)
	}
	if store.UnderRoot(root, filepath.Join(root, "notes.md")) {
		t.Errorf("UnderRoot says %s/notes.md is inside the root; it is a symlink to %s/authorized_keys",
			root, outside)
	}

	// a parent directory is a symlink to a not-yet-existing directory outside
	if err := os.Symlink(filepath.Join(outside, "conf"), filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	if store.UnderRoot(root, filepath.Join(root, "docs", "x.md")) {
		t.Errorf("UnderRoot says %s/docs/x.md is inside the root; docs is a symlink to %s/conf",
			root, outside)
	}
}

// The legitimate shapes must keep answering yes, or the guard turns into a
// sync outage. A mount root that is itself a symlink is the common case
// (/tmp on macOS, a home directory on a mounted volume).
func TestSec_UnderRoot_LegitimateRootsAreStillWritable(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "mount")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "via-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ root, p string }{
		{root, filepath.Join(root, "a.md")},
		{root, filepath.Join(root, "docs", "deep", "new", "a.md")},
		{link, filepath.Join(link, "a.md")},
		{link, filepath.Join(link, "docs", "a.md")},
	} {
		if !store.UnderRoot(tc.root, tc.p) {
			t.Errorf("UnderRoot(%s, %s) = false, want true — a legitimate write is refused", tc.root, tc.p)
		}
	}
}
