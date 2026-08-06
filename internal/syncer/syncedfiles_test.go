package syncer

import (
	"os"
	"path/filepath"
	"testing"
)

// SyncedFiles is what `bdrive grep` searches, so it has to be exactly the set
// the cycle uploads — and it has to get there without descending a pruned
// directory, which is the whole reason it exists instead of Explain.
func TestSyncedFilesMatchesTheSyncSet(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, IgnoreFile, "node_modules/\n*.log\n")
	write(t, a.Folder, "docs/guide.md", "yes")
	write(t, a.Folder, "docs/deep/spec.md", "yes")
	write(t, a.Folder, "README.md", "yes")
	write(t, a.Folder, "debug.log", "no")
	write(t, a.Folder, ".DS_Store", "no")
	write(t, a.Folder, ".bdrive-tmp-x", "no")
	write(t, a.Folder, ".bdrive/config.json", `{}`)
	write(t, a.Folder, ".git/HEAD", "no")
	write(t, a.Folder, "node_modules/pkg/index.js", "no")

	// A nested mount syncs through its own project, not this one.
	nested := filepath.Join(a.Folder, "sub")
	write(t, a.Folder, "sub/inner.md", "own project")
	if err := os.MkdirAll(filepath.Join(nested, ".bdrive"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, a.Folder, "sub/.bdrive/config.json", `{"id":"m-other"}`)

	got, err := SyncedFiles(a.Folder, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, p := range got {
		set[p] = true
	}
	for _, want := range []string{"docs/guide.md", "docs/deep/spec.md", "README.md", IgnoreFile} {
		if !set[want] {
			t.Errorf("%s should sync, got %v", want, got)
		}
	}
	for _, never := range []string{
		"debug.log", ".DS_Store", ".bdrive-tmp-x", ".bdrive/config.json",
		".git/HEAD", "node_modules/pkg/index.js", "sub/inner.md",
	} {
		if set[never] {
			t.Errorf("%s must not be listed, got %v", never, got)
		}
	}

	// The same answer Explain gives, since both go through walkFolder — if
	// these ever disagree, `bdrive grep` and `bdrive scope --explain` are
	// telling the operator two different stories about one folder.
	synced, _, err := Explain(a.Folder, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(synced) != len(got) {
		t.Fatalf("SyncedFiles %v != Explain %v", got, synced)
	}
	for i := range synced {
		if synced[i] != got[i] {
			t.Fatalf("SyncedFiles %v != Explain %v", got, synced)
		}
	}
}

// The reason SyncedFiles is not Explain: Explain counts the files inside a
// pruned directory, so it reads every entry of node_modules/. A grep must not.
func TestSyncedFilesDoesNotDescendPrunedDirs(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, IgnoreFile, "heavy/\n")
	write(t, a.Folder, "keep.md", "yes")
	write(t, a.Folder, "heavy/a.md", "no")
	write(t, a.Folder, "heavy/deep/b.md", "no")

	got, err := SyncedFiles(a.Folder, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p != "keep.md" && p != IgnoreFile {
			t.Fatalf("walked into a pruned dir: %v", got)
		}
	}
}

// A rule a TEAMMATE pushed must not widen what this device reports as synced
// until this device accepts it — the SkipUp asymmetry, which is why accepted
// is a parameter at all.
func TestSyncedFilesHonorsAcceptedRules(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, IgnoreFile, "*.log\n!keep.log\n")
	write(t, a.Folder, "keep.log", "negated")

	// Local rules only: the negation is this device's own file, so it applies.
	got, err := SyncedFiles(a.Folder, nil, "*.log\n!keep.log\n")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range got {
		if p == "keep.log" {
			found = true
		}
	}
	if !found {
		t.Fatalf("accepted negation should sync keep.log, got %v", got)
	}

	// The negation is new and unaccepted: the upload door stays shut.
	got, err = SyncedFiles(a.Folder, nil, "*.log\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p == "keep.log" {
			t.Fatalf("unaccepted negation must not widen the sync set, got %v", got)
		}
	}
}
