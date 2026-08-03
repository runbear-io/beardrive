package main

// Round 9 — the target is round 8's fixes on the client. Specifically the new
// condition in config.ResolveMount, which round 8's own CISO flagged as a
// possible DENIAL PRIMITIVE:
//
//	if registered && !samePath(mi.Path, folder) && mountLivesAt(mi.Path, p.ID) {
//	        return p, false, fmt.Errorf(... "run `bdrive init` here" ...)
//	}
//
// Helpers are prefixed secfx8; no existing file is touched.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// secfx8Enroll creates a project folder and registers it, the way `bdrive
// init` leaves things.
func secfx8Enroll(t *testing.T, folder, volume string) config.Project {
	t.Helper()
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := config.SaveProject(folder, config.Project{Volume: volume, Remote: "file://" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	return p
}

// A mount is keyed by a stable id and "nothing is keyed by the folder path, so
// renames/moves are free — ResolveMount self-heals the registry path". Round 8
// narrowed the self-heal to the case where the recorded path no longer holds
// this mount's config, which correctly stops an arriving COPY from stealing an
// enrolled row.
//
// The cost is that the copy wins by simply existing: whatever sits at the
// recorded path decides whether the real, moved folder is still usable. A
// backup restore, an unfinished `cp -r`, a sync client re-creating a deleted
// directory — or any local process that can write there — re-creates the old
// path, and every bdrive command in the genuinely moved folder now fails:
// status, sync, and the daemon all go through mustProject.
//
// The remedy the error itself names does not work either. Its text ends "run
// `bdrive init` here to connect this folder to a project", and `bdrive init`
// resolves the mount before it does anything else (init.go:133) — so the
// command the error tells the user to run is the command that produces it.
// Nothing in the CLI moves the registry row; the only way out is deleting the
// leftover or hand-editing mounts.json.
func TestSec_Mount_AMovedProjectIsNotStrandedByALeftoverAtTheOldPath(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	base := t.TempDir()
	oldPath := filepath.Join(base, "wiki")
	newPath := filepath.Join(base, "notes")

	secfx8Enroll(t, oldPath, "wiki")

	// Control: before the move, the folder is usable.
	if _, err := seccliRun(t, statusCmd(), []string{oldPath}); err != nil {
		t.Fatalf("control: status on the enrolled folder failed: %v", err)
	}

	// A genuine move, exactly what the docs call free.
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	// Control: with the old path gone, the self-heal works.
	if _, err := seccliRun(t, statusCmd(), []string{newPath}); err != nil {
		t.Fatalf("control: status on the moved folder failed before anything was left behind: %v", err)
	}

	// Undo the registry's knowledge of the move (the ordinary case: the user
	// moved the folder and has not run a bdrive command yet), then let
	// something re-create the old path holding the same settings file.
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	for id, mi := range mounts {
		mi.Path = oldPath
		mounts[id] = mi
	}
	if err := config.SaveMounts(mounts); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(newPath, ".bdrive", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(oldPath, ".bdrive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPath, ".bdrive", "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	_, statusErr := seccliRun(t, statusCmd(), []string{newPath})
	_, initErr := seccliRun(t, initCmd(), []string{newPath, "--yes"})
	if statusErr == nil {
		return // the moved project is still usable: the secure outcome
	}
	t.Errorf("the real, moved project folder is now unusable, and its own named remedy fails too.\n"+
		"  bdrive status %s -> %v\n"+
		"  bdrive init   %s -> %v\n"+
		"Anything that re-creates the recorded path holding this mount's settings file — a backup "+
		"restore, an interrupted copy, a file-sync client putting a deleted directory back, or a "+
		"local process with write access to that parent — strands the genuine folder. Every CLI "+
		"entry point goes through mustProject/ResolveMount, and `bdrive init` (which the error "+
		"tells the user to run) resolves the mount before it does anything else, so it returns the "+
		"same error. There is no command that re-points the row.",
		newPath, statusErr, newPath, strings.TrimSpace(errString(initErr)))
}

func errString(err error) string {
	if err == nil {
		return "<no error>"
	}
	return err.Error()
}
