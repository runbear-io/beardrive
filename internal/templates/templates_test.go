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
			// Every ancestor, not just the direct parent: the rule's own
			// reason is that an empty directory never reaches a teammate, and
			// an intermediate directory on the way to a file is not empty.
			// Marking only the parent made the check stricter than its reason
			// and failed the first template more than one level deep. What it
			// still catches is a genuinely file-less directory, which is all
			// it ever claimed to.
			for d := path.Dir(f.Path); d != "." && d != "/"; d = path.Dir(d) {
				haveFileIn[d] = true
			}

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
		// The three questions an AGENTS.md exists to answer. Each is checked
		// through a set of alternatives, because the honest vocabulary
		// differs by structure: PARA archives, a wiki supersedes and revises
		// stale claims. What must not vary is that the question is answered.
		for question, words := range map[string][]string{
			"where a new file goes":                        {"goes"},
			"what happens when something stops being true": {"rchiv", "supersede", "stale"},
			"what a good filename looks like":              {"ilename"},
		} {
			answered := false
			for _, w := range words {
				answered = answered || strings.Contains(agents, w)
			}
			if !answered {
				t.Errorf("%s: AGENTS.md does not answer %s", tpl.Name, question)
			}
		}
		for _, dir := range tpl.Dirs() {
			if !haveFileIn[dir] {
				t.Errorf("%s: directory %q holds no file, so it never syncs", tpl.Name, dir)
			}
		}
	}
}

// The skills template is the first one whose payload is a dot-directory, and
// go:embed drops paths beginning with "." unless the pattern is prefixed
// `all:` — with no error anywhere. Without this test the template ships with
// only its AGENTS.md and nothing fails.
func TestSkillsTemplateShipsItsDotDirectory(t *testing.T) {
	tpl, err := Get("skills")
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, f := range tpl.Files {
		if strings.HasPrefix(f.Path, ".claude/skills/") && strings.HasSuffix(f.Path, "/SKILL.md") {
			found = f.Path
		}
	}
	if found == "" {
		var paths []string
		for _, f := range tpl.Files {
			paths = append(paths, f.Path)
		}
		t.Fatalf("the skills template carries no .claude/skills/<name>/SKILL.md, only %v — "+
			"check that the embed directive is `//go:embed all:files`; a plain `files` pattern "+
			"drops dot-prefixed paths silently", paths)
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
