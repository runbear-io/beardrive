package templates

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Every shipped template has to satisfy the two things that make it work at
// all: the hub must be able to journal every path (so the paths obey the same
// rules cleanUploadPath enforces on an upload), and every directory must hold
// a file — BearDrive syncs paths, not directories, so an empty directory
// never reaches a teammate.
func TestShippedTemplates(t *testing.T) {
	list := List()
	if len(list) != len(shipped) {
		t.Fatalf("List() returned %d templates, want %d", len(list), len(shipped))
	}
	for _, tpl := range list {
		if tpl.Title == "" || tpl.Blurb == "" {
			t.Errorf("%s: needs a title and a blurb for the menus", tpl.Name)
		}
		var agents string
		haveFileIn := map[string]bool{}
		for _, f := range tpl.Files {
			if f.Path == "AGENTS.md" {
				agents = f.Content
			}
			haveFileIn[path.Dir(f.Path)] = true

			// The same rules cleanUploadPath applies, so the hub can never
			// reject its own content.
			if f.Path == "" || strings.HasPrefix(f.Path, "/") || strings.HasSuffix(f.Path, "/") ||
				path.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, "../") ||
				strings.Contains(f.Path, "/../") {
				t.Errorf("%s: path %q is not a clean relative path", tpl.Name, f.Path)
			}
			if strings.HasPrefix(path.Base(f.Path), ".bdrive") {
				t.Errorf("%s: path %q uses a reserved name", tpl.Name, f.Path)
			}
			if strings.TrimSpace(f.Content) == "" {
				t.Errorf("%s: %s is empty", tpl.Name, f.Path)
			}
		}
		if strings.TrimSpace(agents) == "" {
			t.Errorf("%s: no AGENTS.md at the root — the instructions are the deliverable", tpl.Name)
		}
		// The three questions an AGENTS.md exists to answer.
		for _, want := range []string{"goes", "rchiv", "ilename"} {
			if !strings.Contains(agents, want) {
				t.Errorf("%s: AGENTS.md says nothing about %q", tpl.Name, want)
			}
		}
		for _, dir := range tpl.Dirs() {
			if !haveFileIn[dir] {
				t.Errorf("%s: directory %q holds no file, so it never syncs", tpl.Name, dir)
			}
		}
	}
}

func TestGetUnknownNamesTheSet(t *testing.T) {
	_, err := Get("karpathy-wiki")
	if err == nil {
		t.Fatal("unknown template should error")
	}
	for _, name := range Names() {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not name the valid template %q", err, name)
		}
	}
}

// Seeding twice must be a no-op, not a divergence: WriteTo never overwrites,
// which is what makes `bdrive init --template` re-runnable and a hub-seeded
// project safe for an agent to seed again by mistake.
func TestWriteToSkipsExisting(t *testing.T) {
	tpl, err := Get("docs")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const sentinel = "# mine, not yours\n"
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	wrote, err := tpl.WriteTo(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range wrote {
		if p == "AGENTS.md" {
			t.Fatal("WriteTo reported writing a path that already existed")
		}
	}
	if len(wrote) != len(tpl.Files)-1 {
		t.Fatalf("wrote %d paths, want %d", len(wrote), len(tpl.Files)-1)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md")); string(got) != sentinel {
		t.Fatalf("WriteTo clobbered an existing file: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "decisions", "0001-record-decisions.md")); err != nil {
		t.Fatalf("WriteTo did not create the rest of the template: %v", err)
	}

	// Second pass: nothing left to write.
	again, err := tpl.WriteTo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("re-seeding wrote %v, want nothing", again)
	}
}
