package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreRule(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "notes", ".omc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes", "private.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ arg, want string }{
		{filepath.Join(root, "notes", ".omc"), "notes/.omc/"}, // a directory covers its contents
		{filepath.Join(root, "notes", "private.md"), "notes/private.md"},
		{filepath.Join(root, "gone.txt"), "gone.txt"}, // need not exist
	} {
		got, err := ignoreRule(root, tc.arg)
		if err != nil {
			t.Fatalf("ignoreRule(%s): %v", tc.arg, err)
		}
		if got != tc.want {
			t.Errorf("ignoreRule(%s) = %q, want %q", tc.arg, got, tc.want)
		}
	}

	// Outside the project, and the rules file itself, are errors.
	for _, bad := range []string{filepath.Dir(root), root, filepath.Join(root, "..", "elsewhere"), filepath.Join(root, ".bdriveignore")} {
		if got, err := ignoreRule(root, bad); err == nil {
			t.Errorf("ignoreRule(%s) = %q, want an error", bad, got)
		}
	}
}

func TestAppendIgnoreRules(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".bdriveignore")
	if err := os.WriteFile(path, []byte("*.tmp"), 0o644); err != nil { // no trailing newline
		t.Fatal(err)
	}

	added, err := appendIgnoreRules(root, []string{".omc/", "*.tmp", ".omc/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || !added[".omc/"] {
		t.Fatalf("added = %v, want only .omc/", added)
	}
	if got := string(mustRead(t, path)); got != "*.tmp\n.omc/\n" {
		t.Fatalf("file = %q", got)
	}

	// Idempotent: a second run writes nothing.
	before := mustRead(t, path)
	added, err = appendIgnoreRules(root, []string{".omc/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v on a repeat run", added)
	}
	if string(mustRead(t, path)) != string(before) {
		t.Fatal("repeat run rewrote the file")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
