package templates

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// Round 6 — internal/templates is the zero-test package: project seeding
// writes files into a folder and no round has ever touched it.
//
// Two doors reach WriteTo: `bdrive init --template` (seedLocally, into the
// user's folder) and `bdrive init --template` again on an already-initialized
// folder — which is the agent path, so it runs against a folder whose
// contents were pulled from the hub moments earlier.
//
// Helpers are prefixed sectpl.

// sectplDocs is the recommended shipped template — the one an interactive
// init offers first, so it is the one that actually gets written.
func sectplDocs(t *testing.T) Template {
	t.Helper()
	tpl, err := Get("docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(tpl.Files) == 0 {
		t.Fatal("the docs template ships no files")
	}
	return tpl
}

// TestSec_Templates_SeedingCannotWriteThroughASymlinkedName
//
// WriteTo (templates.go:123) resolves each destination with filepath.Join and
// then calls os.Stat / os.MkdirAll / os.WriteFile. All three follow symlinks,
// and a lexical Join says nothing about what is on disk — which is exactly
// the finding round 4 closed in the syncer ("unsafeRel judges the path's
// SPELLING; MkdirAll/CreateTemp/Rename follow symlinks") and round 4 closed
// again in the file:// backend, both by resolving the boundary on disk with
// store.UnderRoot. WriteTo is a third door into a project folder and it never
// got the guard.
//
// Two shapes, both of which need nothing but a name in the folder that init
// is about to seed:
//
//   - a DANGLING symlink at a template file's own name. os.Stat fails on it
//     (so WriteTo's "already exists, skip" rule does not fire), and
//     os.WriteFile follows it and CREATES the target — anywhere on the disk,
//     with the template's content.
//   - a symlinked DIRECTORY at a template directory's name. MkdirAll walks
//     through it and every file under it lands outside the project.
//
// A symlink gets into that folder the ordinary way a folder gets anything: it
// arrives with the folder (a zip, a clone, a colleague's copy — the same
// premise rounds 4 and 5 used for .bdrive/config.json), or an agent session
// with write access in the project makes one before the human runs init.
// `bdrive init --template` in an already-initialized folder is documented as
// the agent's path and re-running it is documented as safe.
//
// The secure behavior asserted: nothing outside the seeded folder is created
// or modified. Skipping the file, refusing it, or resolving the boundary on
// disk all satisfy it.
func TestSec_Templates_SeedingCannotWriteThroughASymlinkedName(t *testing.T) {
	tpl := sectplDocs(t)

	t.Run("dangling_symlink_at_a_file_name", func(t *testing.T) {
		outside := t.TempDir()
		target := filepath.Join(outside, "authorized_keys")
		folder := t.TempDir()

		victim := tpl.Files[0].Path // a real path this template writes
		if err := os.MkdirAll(filepath.Dir(filepath.Join(folder, victim)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(folder, filepath.FromSlash(victim))); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		if _, err := tpl.WriteTo(folder); err != nil {
			t.Logf("WriteTo returned %v", err)
		}
		if _, err := os.Lstat(target); err == nil {
			t.Errorf("seeding %q followed a dangling symlink and created %s — outside the project",
				victim, target)
		}
	})

	t.Run("symlinked_directory", func(t *testing.T) {
		// Pick a directory the template actually writes into.
		dirs := tpl.Dirs()
		if len(dirs) == 0 {
			t.Skip("the docs template has no subdirectories")
		}
		victimDir := dirs[0]
		if strings.Contains(victimDir, "/") {
			t.Skip("need a top-level directory to symlink")
		}
		outside := t.TempDir()
		folder := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(folder, victimDir)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		if _, err := tpl.WriteTo(folder); err != nil {
			t.Logf("WriteTo returned %v", err)
		}
		ents, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) > 0 {
			var names []string
			for _, e := range ents {
				names = append(names, e.Name())
			}
			t.Errorf("seeding wrote %v through the symlinked directory %q into %s — outside the project",
				names, victimDir, outside)
		}
	})
}

// TestSec_Templates_AFilePathCannotEscapeTheProjectRoot
//
// Template and File are exported with exported fields, and WriteTo does
// nothing but Join a File.Path onto the destination. Every other write door
// in the product validates its destination first — cleanUploadPath on the
// hub, unsafeRel plus store.UnderRoot in the syncer — and this one has no
// check at all, so the only thing standing between it and an absolute or
// traversing path is that today's callers happen to pass the go:embed'ed set.
// That is the same argument round 4 rejected for the upload/sync pair: the
// guard belongs where the write happens.
//
// The secure behavior asserted: WriteTo refuses a path it cannot write inside
// the folder it was given, and creates nothing when it does.
func TestSec_Templates_AFilePathCannotEscapeTheProjectRoot(t *testing.T) {
	for _, bad := range []string{
		"../escaped.md",
		"../../../../tmp/escaped.md",
		"docs/../../escaped.md",
	} {
		t.Run(bad, func(t *testing.T) {
			parent := t.TempDir()
			folder := filepath.Join(parent, "project", "deep")
			if err := os.MkdirAll(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			tpl := Template{Name: "hostile", Files: []File{{Path: bad, Content: "x"}}}
			if _, err := tpl.WriteTo(folder); err == nil {
				t.Errorf("WriteTo accepted %q", bad)
			}
			// Whatever it returned, nothing may exist outside folder.
			abs := filepath.Join(folder, filepath.FromSlash(bad))
			if rel, err := filepath.Rel(folder, abs); err == nil && strings.HasPrefix(rel, "..") {
				if _, err := os.Lstat(abs); err == nil {
					t.Errorf("WriteTo created %s, outside %s", abs, folder)
				}
			}
		})
	}
}

// TestSec_Templates_ShippedPathsAreAllInsideTheRootAndNotReserved asserts the
// property the packaging relies on: no shipped template names a path that
// escapes the project, is absolute, or lands in a reserved directory
// (.bdrive/, .git/ — the set both the hub's upload door and the syncer's
// materialize refuse). This is the assertion that keeps a future template
// directory from becoming the input the two tests above are about.
func TestSec_Templates_ShippedPathsAreAllInsideTheRootAndNotReserved(t *testing.T) {
	for _, tpl := range List() {
		for _, f := range tpl.Files {
			switch {
			case f.Path == "":
				t.Errorf("%s: empty path", tpl.Name)
			case strings.HasPrefix(f.Path, "/"):
				t.Errorf("%s: absolute path %q", tpl.Name, f.Path)
			case path.Clean(f.Path) != f.Path:
				t.Errorf("%s: unclean path %q", tpl.Name, f.Path)
			case strings.HasPrefix(f.Path, "../"):
				t.Errorf("%s: escaping path %q", tpl.Name, f.Path)
			case config.ReservedPath(f.Path):
				t.Errorf("%s: reserved path %q", tpl.Name, f.Path)
			}
			for _, r := range f.Path {
				if r < 0x20 || r == 0x7f {
					t.Errorf("%s: control character in path %q", tpl.Name, f.Path)
					break
				}
			}
		}
	}
}
