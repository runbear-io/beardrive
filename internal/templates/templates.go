// Package templates holds the starting structures a new project can be
// created from: a directory skeleton plus the AGENTS.md that explains it.
//
// The files are go:embed'ed rather than fetched, because `cmd/bdrive` is one
// binary serving the CLI, the daemon and the hub — so the hub that seeds a
// project at creation and the client that seeds one with `bdrive init
// --template` read the identical set, with no gallery and no drift.
//
// The AGENTS.md is the deliverable. Directories are the easy half and the
// half that decays; what makes a structure stick is the instruction file
// telling an agent where a new note goes, when something is archived, and
// what a good filename looks like.
//
// One hard rule for the content: BearDrive syncs paths, not directories
// (internal/syncer/walk.go only journals regular files), so an empty
// directory never reaches a teammate. Every directory in a template holds at
// least one real file — asserted in the tests.
package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed files
var content embed.FS

// File is one file of a template: a slash-separated path relative to the
// project root, and its literal content.
type File struct {
	Path    string `json:"path"`
	Content string `json:"-"`
}

// Template is one starting structure.
type Template struct {
	Name  string `json:"name"`  // the flag/API value, e.g. "docs"
	Title string `json:"title"` // what a menu shows, e.g. "Plain docs + ADRs"
	Blurb string `json:"blurb"` // the one-line shape, e.g. "docs/, decisions/"
	Files []File `json:"-"`
}

// shipped is the registry, in the order both surfaces render: the
// recommended one first. Adding a template is a directory under files/ plus
// a row here — no other code.
var shipped = []struct{ Name, Title, Blurb string }{
	{"docs", "Plain docs + ADRs", "docs/, decisions/"},
	{"para", "PARA", "projects/, areas/, resources/, archives/"},
}

// List returns every shipped template, recommended first.
func List() []Template {
	out := make([]Template, 0, len(shipped))
	for _, s := range shipped {
		t, err := Get(s.Name)
		if err != nil {
			// Unreachable: the files are embedded in this binary. Panicking
			// here would take a hub down for a packaging mistake.
			continue
		}
		out = append(out, t)
	}
	return out
}

// Names lists the valid template names, for error messages.
func Names() []string {
	out := make([]string, len(shipped))
	for i, s := range shipped {
		out[i] = s.Name
	}
	return out
}

// Get returns the named template. An unknown name errors, naming the set.
func Get(name string) (Template, error) {
	for _, s := range shipped {
		if s.Name != name {
			continue
		}
		files, err := load(name)
		if err != nil {
			return Template{}, err
		}
		return Template{Name: s.Name, Title: s.Title, Blurb: s.Blurb, Files: files}, nil
	}
	return Template{}, fmt.Errorf("unknown template %q (valid: %s)", name, strings.Join(Names(), ", "))
}

// load reads one template's files out of the embedded set, sorted by path so
// seeding order is stable (and so a journal's op order is reproducible).
func load(name string) ([]File, error) {
	root := "files/" + name
	var out []File
	err := fs.WalkDir(content, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := content.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, File{Path: strings.TrimPrefix(p, root+"/"), Content: string(b)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// WriteTo writes the template into dir and returns the paths it wrote. A path
// that already exists is never overwritten and is left out of the result —
// which is what makes seeding twice a no-op rather than a divergence.
func (t Template) WriteTo(dir string) ([]string, error) {
	var wrote []string
	for _, f := range t.Files {
		abs := filepath.Join(dir, filepath.FromSlash(f.Path))
		if _, err := os.Stat(abs); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return wrote, err
		}
		if err := os.WriteFile(abs, []byte(f.Content), 0o644); err != nil {
			return wrote, err
		}
		wrote = append(wrote, f.Path)
	}
	return wrote, nil
}

// Dirs lists every directory a template's paths imply, for the
// every-directory-holds-a-file check.
func (t Template) Dirs() []string {
	seen := map[string]bool{}
	for _, f := range t.Files {
		for d := path.Dir(f.Path); d != "." && d != "/"; d = path.Dir(d) {
			seen[d] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
