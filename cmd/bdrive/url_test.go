package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// bdrive url computes the permission-walled viewer link locally: hub origin
// + project id from the mount's remote, path segments percent-encoded with
// literal "/" separators, unsynced paths refused.
func TestURLCommand(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678",
	}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(folder, "wiki notes"), 0o755)
	os.WriteFile(filepath.Join(folder, "wiki notes", "a report.md"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(folder, ".bdriveignore"), []byte("drafts/\n"), 0o644)

	run := func(args ...string) (string, error) {
		c := urlCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs(args)
		err := c.Execute()
		return strings.TrimSpace(out.String()), err
	}

	// A file: segments encoded, slashes literal.
	got, err := run(filepath.Join(folder, "wiki notes", "a report.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://hub.example.com/p-12345678/wiki%20notes/a%20report.md"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	// The project root: the home page.
	if got, err = run(folder); err != nil || got != "https://hub.example.com/p-12345678" {
		t.Fatalf("root url = %q, %v", got, err)
	}

	// An ignored path is refused — the link would 404 for everyone.
	if _, err = run(filepath.Join(folder, "drafts", "wip.md")); err == nil || !strings.Contains(err.Error(), "not synced") {
		t.Fatalf("ignored path: err = %v, want 'not synced'", err)
	}

	// Outside the project entirely.
	if _, err = run(filepath.Join(t.TempDir(), "elsewhere.md")); err == nil {
		t.Fatal("outside path should error")
	}
}

// A --shared style mount (include list) refuses links outside the scope.
func TestURLCommandIncludeScope(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	if _, err := config.SaveProject(folder, config.Project{
		Volume:  "wiki",
		Remote:  "https://hub.example.com/p/p-12345678",
		Include: []string{"wiki/"},
	}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(folder, "wiki"), 0o755)
	os.WriteFile(filepath.Join(folder, "wiki", "a.md"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(folder, "code.go"), []byte("x"), 0o644)

	run := func(arg string) (string, error) {
		c := urlCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs([]string{arg})
		err := c.Execute()
		return strings.TrimSpace(out.String()), err
	}
	if got, err := run(filepath.Join(folder, "wiki", "a.md")); err != nil || got != "https://hub.example.com/p-12345678/wiki/a.md" {
		t.Fatalf("in-scope = %q, %v", got, err)
	}
	if _, err := run(filepath.Join(folder, "code.go")); err == nil || !strings.Contains(err.Error(), "not synced") {
		t.Fatalf("out-of-scope: err = %v, want 'not synced'", err)
	}
}
