package syncer

import (
	"strings"
	"testing"
)

// Measure exists to size the warning `bdrive init` prints, so the one thing
// it must get right is that it counts what would REALLY sync. A folder whose
// bulk is already ignored must measure small, or the warning fires on every
// ordinary repo and people learn to scroll past it.
func TestMeasureCountsOnlyWhatSyncs(t *testing.T) {
	d := newDevice(t, "deva", nil)
	write(t, d.Folder, IgnoreFile, "node_modules/\n*.mp4\n")
	write(t, d.Folder, "notes.md", "hello")                                   // 5 B, syncs
	write(t, d.Folder, "docs/guide.md", "guide")                              // 5 B, syncs
	write(t, d.Folder, "node_modules/dep/index.js", strings.Repeat("x", 900)) // ignored dir
	write(t, d.Folder, "demo.mp4", strings.Repeat("y", 900))                  // ignored glob
	write(t, d.Folder, ".git/objects/ab/cdef", strings.Repeat("z", 900))      // never syncs, built in

	files, bytes, err := Measure(d.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	// notes.md + docs/guide.md + the .bdriveignore itself, which does sync.
	if files != 3 {
		t.Fatalf("files = %d, want 3 (the ignored/reserved paths must not count)", files)
	}
	if bytes >= 900 {
		t.Fatalf("bytes = %d — an ignored 900 B file was counted", bytes)
	}
}

// A scope narrowing is written as .bdriveignore negation rules, so Measure
// has to shrink with it: otherwise `bdrive scope add` would never quiet the
// warning it tells you to run.
func TestMeasureShrinksWithScope(t *testing.T) {
	d := newDevice(t, "devb", nil)
	write(t, d.Folder, ".bdrive/config.json", `{"include":["/docs/"]}`)
	write(t, d.Folder, "docs/guide.md", "in scope")
	write(t, d.Folder, "private/secrets.txt", strings.Repeat("q", 500))

	all, allBytes, err := Measure(d.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	scoped, scopedBytes, err := Measure(d.Folder, []string{"/docs/"})
	if err != nil {
		t.Fatal(err)
	}
	if scoped >= all || scopedBytes >= allBytes {
		t.Fatalf("scoped measure (%d files, %d B) did not shrink vs whole folder (%d files, %d B)",
			scoped, scopedBytes, all, allBytes)
	}
	if scopedBytes >= 500 {
		t.Fatalf("out-of-scope file counted: %d B", scopedBytes)
	}
}
