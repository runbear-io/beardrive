package config_test

// Round 10, row 17 — round 9's own fix: MountInfo.Dev+Ino, the move-vs-copy
// discriminator.
//
// Helpers are prefixed secfx9; secpkgHome is reused from sec_config_test.go.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// secfx9Enroll writes a folder carrying p's settings and resolves it once, so
// the registry holds the row a real `bdrive init` would have written —
// including the dev+ino round 9 added.
func secfx9Enroll(t *testing.T, folder string, p config.Project) {
	t.Helper()
	dir := filepath.Join(folder, config.ProjectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatalf("enrol %s: %v", folder, err)
	}
}

// secfx9Row returns the registry row for a mount id.
func secfx9Row(t *testing.T, id string) config.MountInfo {
	t.Helper()
	m, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	return m[id]
}

// ---------------------------------------------------------------------------
// Round 9 made dev+ino the discriminator between "this mount moved" and "this
// is a copy of it". The row it lives in is still overwritten by whichever
// folder resolves while the recorded path happens not to answer.
//
// mountLivesAt() is a LoadProject of the recorded path, so it says "no" for
// every ordinary reason a path stops answering for a moment: an external or
// network volume not mounted yet at login, a folder being renamed, a restore
// in progress. In that window a folder holding a COPY of the settings — an
// unpacked archive, a clone, a colleague's zip — takes the row, and its own
// dev+ino are written over the identity that was the whole point of the field.
//
// When the real folder comes back it is now the one that does not match, the
// copy's path does hold this mount's config, and the real project is refused
// permanently — including by `bdrive init`, the remedy the error names. That
// is round 9's strand-a-project bug with an attacker in it.
//
// The delta: the identical copy, resolved while the real folder IS readable,
// is correctly refused and the row is untouched.
// ---------------------------------------------------------------------------

func TestSec_Mounts_ACopyCannotTakeTheRowWhileTheRealFolderIsUnreadable(t *testing.T) {
	secpkgHome(t)
	p := config.Project{ID: "m-real", Volume: "vol", Remote: "https://hub.example/p/proj"}

	real := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	secfx9Enroll(t, real, p)
	row := secfx9Row(t, p.ID)
	if row.Path != real || row.Ino == 0 {
		t.Fatalf("fixture: row = %+v, want the real path with a recorded identity", row)
	}

	// The copy: byte-identical settings in a different directory. Resolved
	// while the real folder answers, it is correctly refused and changes
	// nothing — this is the control.
	copyDir := filepath.Join(t.TempDir(), "unpacked")
	if err := os.MkdirAll(filepath.Join(copyDir, config.ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(copyDir, config.ProjectDir, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.ResolveMount(copyDir); err == nil {
		t.Fatalf("control is broken: the copy was accepted with the real folder in place")
	}
	if got := secfx9Row(t, p.ID); got.Path != real {
		t.Fatalf("control is broken: the refused copy still moved the row to %s", got.Path)
	}

	// The real folder stops answering for a moment — an unmounted volume, a
	// rename, a restore in flight. Nothing about it is destroyed.
	stash := real + ".away"
	if err := os.Rename(real, stash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.ResolveMount(copyDir); err != nil {
		t.Logf("copy resolved with an error (good sign): %v", err)
	}
	if err := os.Rename(stash, real); err != nil {
		t.Fatal(err)
	}

	// The real project is back at the path it never left.
	_, ok, err := config.ResolveMount(real)
	if err != nil || !ok {
		t.Errorf("the real project at %s is now refused: ok=%v err=%v.\n"+
			"A folder holding a copy of the settings resolved during a window in which the "+
			"recorded path did not answer, took the registry row, and overwrote the dev+ino "+
			"that identifies the real directory. The real folder is now the one that cannot "+
			"prove it is the mount, and `bdrive init` — the remedy the message names — resolves "+
			"the mount first and fails the same way. Registry row now: %+v",
			real, ok, err, secfx9Row(t, p.ID))
	}
	if got := secfx9Row(t, p.ID); got.Path != real {
		t.Errorf("registry row points at %s, not the real project at %s", got.Path, real)
	}
}

// A row written before round 9 carries Dev=0/Ino=0, and `moved` requires a
// non-zero dev — so for every mount already enrolled on every machine today,
// the discriminator is absent and the strand round 9 set out to remove is
// exactly as reachable as it was in round 8. The registry has to backfill an
// identity for a row it can still see, or the fix ships inert.
func TestSec_Mounts_ALegacyRowGetsAnIdentityBeforeItIsNeeded(t *testing.T) {
	secpkgHome(t)
	p := config.Project{ID: "m-legacy", Volume: "vol", Remote: "https://hub.example/p/proj"}
	folder := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	secfx9Enroll(t, folder, p)

	// Strip the identity, which is how every mounts.json on disk today reads.
	m, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	mi := m[p.ID]
	mi.Dev, mi.Ino = 0, 0
	m[p.ID] = mi
	if err := config.SaveMounts(m); err != nil {
		t.Fatal(err)
	}

	// A genuine move: same filesystem, os.Rename, so the directory keeps its
	// inode and IS the one the row was written for.
	moved := folder + "-moved"
	if err := os.Rename(folder, moved); err != nil {
		t.Fatal(err)
	}
	// And the old path comes back holding the same settings — the backup
	// restore / file-sync client round 9's comment names as the trigger.
	if err := os.MkdirAll(filepath.Join(folder, config.ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(folder, config.ProjectDir, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := config.ResolveMount(moved); err != nil || !ok {
		t.Errorf("the genuinely moved project at %s is stranded on a legacy registry row: ok=%v err=%v.\n"+
			"Round 9's dev+ino discriminator is only consulted when the row HAS one, and no row "+
			"written before round 9 does — so on every machine that upgrades, the strand bug is "+
			"unchanged until something happens to re-resolve the mount at its old path first.",
			moved, ok, err)
	}
}
