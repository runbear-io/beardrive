package syncer

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// TestExplainMatchesScan is the whole point of `bdrive scope --explain`: the
// paths it calls "synced" must be exactly the paths the sync cycle journals,
// and exactly the paths that show up on a peer device. If these ever diverge
// the command is lying in a trust surface, so this asserts the identity from
// both ends for a whole-folder project and for a scoped one.
func TestExplainMatchesScan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		include []string
		setup   func(t *testing.T, folder string)
	}{
		{
			name: "whole folder",
			setup: func(t *testing.T, folder string) {
				write(t, folder, IgnoreFile, "node_modules/\n*.log\n")
			},
		},
		{
			// PruneDir refuses to prune when an include list exists, so the
			// walk descends node_modules in full — this is the case the
			// collapse pass has to handle.
			name:    "scoped",
			include: []string{"/docs/"},
			setup: func(t *testing.T, folder string) {
				write(t, folder, ".bdrive/config.json", `{"include":["/docs/"]}`)
				write(t, folder, IgnoreFile, "*.log\n")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := sharedRemote(t)
			a := newDevice(t, "deva", be)
			b := newDevice(t, "devb", be)
			tc.setup(t, a.Folder)

			write(t, a.Folder, "docs/guide.md", "synced")
			write(t, a.Folder, "docs/deep/spec.md", "synced too")
			write(t, a.Folder, "src/main.go", "maybe")
			write(t, a.Folder, "scratch/notes.md", "maybe")
			write(t, a.Folder, "debug.log", "never")
			write(t, a.Folder, ".DS_Store", "never")
			write(t, a.Folder, ".bdrive-tmp-x", "never")
			write(t, a.Folder, ".git/HEAD", "never")
			for i := 0; i < 12; i++ {
				write(t, a.Folder, filepath.Join("node_modules/pkg", "f"+string(rune('a'+i))+".js"), "never")
			}
			write(t, a.Folder, "vendor/acme/.bdrive/config.json", `{"mount_id":"m-nested"}`)
			write(t, a.Folder, "vendor/acme/inner.md", "own project")
			if err := os.Symlink(filepath.Join(a.Folder, "docs/guide.md"), filepath.Join(a.Folder, "docs/link.md")); err != nil {
				t.Fatal(err)
			}

			synced, notSynced, err := Explain(a.Folder, tc.include, "")
			if err != nil {
				t.Fatal(err)
			}

			// 1. explain's synced set == the paths scan journals.
			cycle(t, a)
			ops, err := a.Store.DeviceOps(a.Device.ID)
			if err != nil {
				t.Fatal(err)
			}
			var put []string
			for _, op := range ops {
				if op.Kind == journal.KindPut {
					put = append(put, op.Path)
				}
			}
			sort.Strings(put)
			if !reflect.DeepEqual(synced, put) {
				t.Fatalf("explain synced != journaled puts\n explain: %v\n scan:    %v", synced, put)
			}

			// 2. and == what actually landed on a second device.
			cycle(t, b)
			var got []string
			filepath.WalkDir(b.Folder, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(b.Folder, p)
				got = append(got, filepath.ToSlash(rel))
				return nil
			})
			sort.Strings(got)
			if !reflect.DeepEqual(synced, got) {
				t.Fatalf("explain synced != files on peer\n explain: %v\n peer:    %v", synced, got)
			}

			// 3. the nested mount is annotated, never a plain exclusion.
			nested := find(notSynced, "vendor/acme/")
			if nested == nil {
				t.Fatalf("nested mount missing from not-synced: %v", notSynced)
			}
			if !nested.Nested {
				t.Fatal("nested mount must be marked Nested, not listed as excluded")
			}

			// 4. node_modules collapses to one counted line.
			nm := find(notSynced, "node_modules/")
			if nm == nil || nm.Files != 12 {
				t.Fatalf("node_modules should be one entry with 12 files, got %+v (all: %v)", nm, notSynced)
			}
			if len(notSynced) > 12 {
				t.Fatalf("not-synced list should stay small, got %d entries: %v", len(notSynced), notSynced)
			}

			// the builtin exclusions are visible — that is the reassurance
			for _, want := range []string{".git/", ".DS_Store", ".bdrive-tmp-x", "docs/link.md"} {
				if find(notSynced, want) == nil {
					t.Fatalf("%s missing from not-synced: %v", want, notSynced)
				}
			}

			// 5. byte-stable across runs.
			synced2, notSynced2, err := Explain(a.Folder, tc.include, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(synced, synced2) || !reflect.DeepEqual(notSynced, notSynced2) {
				t.Fatal("Explain output is not stable across runs")
			}
		})
	}
}

// TestExplainScopedCollapsesUnsharedDirs pins the scoped-project shape: a
// directory outside the include list prints as one counted line rather than
// every file under it.
func TestExplainScopedCollapsesUnsharedDirs(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, ".bdrive/config.json", `{"include":["/docs/"]}`)
	write(t, a.Folder, "docs/guide.md", "synced")
	for i := 0; i < 5; i++ {
		write(t, a.Folder, filepath.Join("private/deep", "f"+string(rune('a'+i))+".txt"), "no")
	}

	_, notSynced, err := Explain(a.Folder, []string{"/docs/"}, "")
	if err != nil {
		t.Fatal(err)
	}
	e := find(notSynced, "private/")
	if e == nil || e.Files != 5 {
		t.Fatalf("private/ should collapse to one line with 5 files, got %+v (all: %v)", e, notSynced)
	}
	if n := NotSyncedFiles(notSynced); n < 5 {
		t.Fatalf("NotSyncedFiles = %d, should count the collapsed subtree", n)
	}
}

func find(entries []Entry, path string) *Entry {
	for i, e := range entries {
		if e.Path == path {
			return &entries[i]
		}
	}
	return nil
}
