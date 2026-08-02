package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// Round 6, internal/syncer: the four exported functions no TestSec_* named
// (Restore, Explain, NotSyncedFiles, PruneDir) plus the one input every cycle
// reads before it does anything — the per-mount state cache.

// secapiOps returns every op this device has journalled.
func secapiOps(t *testing.T, s *Session) []journal.Op {
	t.Helper()
	ops, err := s.Store.DeviceOps(s.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// unsafeRel is the rule the whole package is built on: "scan only ever
// produces clean relative paths", so materialize/pull enforce it on ops that
// arrive from a peer and the commit side never re-checks its own.
//
// That is true of the WALK, and the walk is not the only thing that mints ops.
// scan's second pass turns every key in state-<mount>.json that the walk did
// not see into a delete op, filtered by .bdriveignore alone — no unsafeRel, no
// neverSync. The cache is a plain JSON file in $BDRIVE_HOME, so anything
// running as the user (an agent session, a dependency's install script, an
// older bdrive that cached a path today's guards would refuse) chooses those
// keys, and this device signs and pushes the result to the hub as its own.
func TestSec_Cycle_ACorruptStateCacheCannotPublishOpsOutsideTheMount(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))

	// Control: an ordinary delete really does become an op, so the pass under
	// attack is the one running.
	write(t, a.Folder, "notes/hello.md", "hi")
	cycle(t, a)
	if err := os.Remove(filepath.Join(a.Folder, "notes", "hello.md")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	deletes := 0
	for _, op := range secapiOps(t, a) {
		if op.Kind == journal.KindDelete {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("control: %d delete ops after removing one file, want 1", deletes)
	}

	// Attack: keys the walk will never see, naming paths no journal may name.
	cache, err := a.Store.LoadCache(a.mountID())
	if err != nil {
		t.Fatal(err)
	}
	hostile := []string{"../../../../etc/beardrive-owned", "..", ".bdrive/config.json", "notes/../../escape.md"}
	for _, rel := range hostile {
		cache[rel] = store.CachedFile{Blob: strings.Repeat("a", 64), Size: 1}
	}
	if err := a.Store.SaveCache(a.mountID(), cache); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)

	for _, op := range secapiOps(t, a) {
		if unsafeRel(op.Path) || neverSync(op.Path) {
			t.Errorf("this device published an op naming %q — a path unsafeRel/neverSync refuse "+
				"everywhere else in the package (kind %s)", op.Path, op.Kind)
		}
	}
}

// Restore re-puts a historical blob as an ordinary local edit and is reachable
// from the CLI as well as the hub route (round 3 tested the ROUTE). The path
// and the sha are both arguments, so both are attacker-shaped whenever the
// history list they come from is.
func TestSec_Restore_PathAndShaStayInsideTheMount(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "notes/hello.md", "hi")
	cycle(t, a)

	var sha string
	for _, op := range secapiOps(t, a) {
		if op.Path == "notes/hello.md" {
			sha = op.Blob
		}
	}
	if sha == "" {
		t.Fatal("fixture: no blob for the seeded file")
	}

	// Control: an ordinary restore works, so a refusal below means something.
	write(t, a.Folder, "notes/hello.md", "clobbered")
	if err := a.Restore(context.Background(), "notes/hello.md", sha); err != nil {
		t.Fatalf("control: ordinary restore: %v", err)
	}
	if got := read(t, a.Folder, "notes/hello.md"); got != "hi" {
		t.Fatalf("control: restore wrote %q", got)
	}

	outside := filepath.Join(filepath.Dir(a.Folder), "outside.md")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlinked directory inside the mount is the shape round 4 closed for
	// materialize; Restore takes the same writeFile, so it must hold here too.
	if err := os.Symlink(filepath.Dir(a.Folder), filepath.Join(a.Folder, "link")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"../outside.md", "../../outside.md", "/tmp/beardrive-restore", "link/outside.md", ".bdrive/config.json"} {
		if err := a.Restore(context.Background(), p, sha); err == nil {
			t.Errorf("Restore(%q) was accepted", p)
		}
	}
	if got, _ := os.ReadFile(outside); string(got) != "private" {
		t.Errorf("a restore rewrote a file outside the mount: %q", got)
	}

	// A sha that is not a hash must not become a storage key either: it is
	// concatenated onto "blobs/" and handed to the backend.
	for _, s := range []string{"../journal/deva.jsonl", "..", "", strings.Repeat("z", 64)} {
		if err := a.Restore(context.Background(), "notes/hello.md", s); err == nil {
			t.Errorf("Restore accepted sha %q", s)
		}
	}
	if got := read(t, a.Folder, "notes/hello.md"); got != "hi" {
		t.Errorf("a refused restore still rewrote the file: %q", got)
	}
}

// Explain is the read side of the same walk. It must not report a path the
// cycle would refuse to sync, and — since it is the CLI's `scope --explain`
// listing — it must agree with the walk about what syncs.
func TestSec_Explain_ReportsNothingTheCycleWouldRefuse(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, "notes/hello.md", "hi")
	write(t, a.Folder, ".git/hooks/pre-commit", "#!/bin/sh\n")
	write(t, a.Folder, ".bdrive/config.json", "{}")
	write(t, a.Folder, "vendor/big.bin", "x")
	write(t, a.Folder, IgnoreFile, "vendor/\n")

	synced, notSynced, err := Explain(a.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced) == 0 {
		t.Fatal("control: Explain reported nothing as synced")
	}
	for _, p := range synced {
		if unsafeRel(p) || neverSync(p) {
			t.Errorf("Explain lists %q as synced, but the cycle refuses it", p)
		}
	}
	for _, e := range notSynced {
		rel := strings.TrimSuffix(e.Path, "/")
		if unsafeRel(rel) {
			t.Errorf("Explain lists %q, which is not a path inside the mount at all", e.Path)
		}
	}
	if n := NotSyncedFiles(notSynced); n < 0 {
		t.Errorf("NotSyncedFiles = %d", n)
	}
}
