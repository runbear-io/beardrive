package syncer

import (
	"os"
	"path/filepath"
	"testing"
)

func filterFrom(t *testing.T, ignore string, include []string) *Filter {
	t.Helper()
	dir := t.TempDir()
	if ignore != "" {
		if err := os.WriteFile(filepath.Join(dir, IgnoreFile), []byte(ignore), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := loadFilter(dir, include)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestIgnorePatterns(t *testing.T) {
	cases := []struct {
		ignore string
		path   string
		want   bool
	}{
		{"*.log", "a.log", true},
		{"*.log", "sub/dir/a.log", true},
		{"*.log", "a.log.txt", false},
		{"# comment\n\n*.tmp", "x.tmp", true},
		{"build/", "build/out.bin", true},
		{"build/", "build", false}, // dir-only pattern must not match a file
		{"build/", "sub/build/x", true},
		{"/docs", "docs/a.md", true},
		{"/docs", "sub/docs/a.md", false}, // anchored
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/sub/a.md", false}, // * stays within a segment
		{"docs/**/a.md", "docs/x/y/a.md", true},
		{"secret?.txt", "secret1.txt", true},
		{"secret?.txt", "secret10.txt", false},
		{"*.log\n!keep.log", "keep.log", false},
		{"*.log\n!keep.log", "other.log", true},
	}
	for _, c := range cases {
		f := filterFrom(t, c.ignore, nil)
		if got := f.Skip(c.path); got != c.want {
			t.Errorf("ignore %q: Skip(%q) = %v, want %v", c.ignore, c.path, got, c.want)
		}
	}
}

func TestIncludePatterns(t *testing.T) {
	f := filterFrom(t, "", []string{"docs/", "*.md"})
	for path, want := range map[string]bool{
		"docs/deep/x.bin": false, // under an included dir
		"README.md":       false, // matches *.md anywhere
		"notes/plan.md":   false,
		"src/main.go":     true, // matches nothing → skipped
	} {
		if got := f.Skip(path); got != want {
			t.Errorf("Skip(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIgnoreBeatsInclude(t *testing.T) {
	f := filterFrom(t, "docs/private.md", []string{"docs/"})
	if !f.Skip("docs/private.md") {
		t.Error("ignored file inside an included dir should be skipped")
	}
	if f.Skip("docs/public.md") {
		t.Error("non-ignored file inside an included dir should sync")
	}
}

func TestPruneDir(t *testing.T) {
	f := filterFrom(t, "node_modules/", nil)
	if !f.PruneDir("node_modules") || !f.PruneDir("sub/node_modules") {
		t.Error("plain dir ignore should prune the walk")
	}
	if f.PruneDir("src") {
		t.Error("unmatched dir must not be pruned")
	}
	// negations and includes make pruning unsafe
	if filterFrom(t, "node_modules/\n!node_modules/keep.js", nil).PruneDir("node_modules") {
		t.Error("must not prune when ! rules exist")
	}
	if filterFrom(t, "node_modules/", []string{"docs/"}).PruneDir("node_modules") {
		t.Error("must not prune when an include list exists")
	}
}
