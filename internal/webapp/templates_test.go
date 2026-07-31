package webapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/templates"
)

// Creating with a template seeds the files through the hub's own write path,
// so a browser-created project arrives structured and every device just
// pulls it.
func TestProjectCreateWithTemplate(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	h := srv.Handler()

	rec := do(t, h, "POST", "/api/projects", map[string]string{"name": "brain", "template": "para"})
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
		Created bool    `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Created || out.Project.Template != "para" {
		t.Fatalf("create response = %+v, want created with template para", out)
	}

	// The record carries it, so a later init can't seed a second copy.
	rec = do(t, h, "GET", "/api/projects/"+out.Project.ID, nil)
	var got Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Template != "para" {
		t.Fatalf("GET /api/projects/{id} template = %q, want para", got.Template)
	}

	// Every template path is in the tree, journaled under the hub's device.
	tpl, err := templates.Get("para")
	if err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, "GET", "/api/p/"+out.Project.ID+"/history", nil)
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var hist struct {
		Entries []struct {
			Path   string     `json:"path"`
			Device DeviceInfo `json:"device"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, e := range hist.Entries {
		seen[e.Path] = e.Device.ID
	}
	if len(hist.Entries) != len(tpl.Files) {
		t.Fatalf("history has %d entries, want one per template file (%d): %v",
			len(hist.Entries), len(tpl.Files), seen)
	}
	for _, f := range tpl.Files {
		if seen[f.Path] == "" {
			t.Fatalf("%s missing from the project: %v", f.Path, seen)
		}
		if seen[f.Path] != webDevice.ID {
			t.Fatalf("%s attributed to %q, want the hub's own device %q", f.Path, seen[f.Path], webDevice.ID)
		}
	}
	if !treePaths(t, h, out.Project.ID)["AGENTS.md"] {
		t.Fatal("the seeded AGENTS.md is not in the file tree")
	}
}

// An unknown template must be caught before anything is created.
func TestProjectCreateUnknownTemplate(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	h := srv.Handler()
	before := len(srv.Projects.List())

	rec := do(t, h, "POST", "/api/projects", map[string]string{"name": "nope", "template": "karpathy-wiki"})
	if rec.Code != 400 {
		t.Fatalf("unknown template: %d %s, want 400", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "docs") || !strings.Contains(rec.Body.String(), "para") {
		t.Fatalf("the 400 should name the valid set: %s", rec.Body)
	}
	if now := len(srv.Projects.List()); now != before {
		t.Fatalf("a rejected template still created a project (%d → %d)", before, now)
	}
}

// No template key at all is exactly today's behavior: an empty project.
func TestProjectCreateWithoutTemplateStaysEmpty(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	h := srv.Handler()

	rec := do(t, h, "POST", "/api/projects", map[string]string{"name": "plain"})
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Project.Template != "" {
		t.Fatalf("template = %q on a plain create", out.Project.Template)
	}
	if paths := treePaths(t, h, out.Project.ID); len(paths) != 0 {
		t.Fatalf("a plain create is not empty any more: %v", paths)
	}
}

// A read-only hub refuses creation before it can seed anything — the
// existing Upload.Enabled guard, unchanged.
func TestProjectCreateWithTemplateReadOnly(t *testing.T) {
	srv, _, _ := newHub(t, false, nil)
	rec := do(t, srv.Handler(), "POST", "/api/projects", map[string]string{"name": "ro", "template": "docs"})
	if rec.Code != 403 {
		t.Fatalf("read-only hub: %d %s, want 403", rec.Code, rec.Body)
	}
}

// The dialog's options come from the server, so a hub shipping another
// template needs no frontend change.
func TestConfigListsTemplates(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	rec := do(t, srv.Handler(), "GET", "/api/config", nil)
	var cfg struct {
		Templates []struct{ Name, Title, Blurb string } `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Templates) != len(templates.List()) || cfg.Templates[0].Name != "docs" {
		t.Fatalf("/api/config templates = %+v", cfg.Templates)
	}
	for _, tpl := range cfg.Templates {
		if tpl.Title == "" || tpl.Blurb == "" {
			t.Fatalf("template %q has nothing to render: %+v", tpl.Name, tpl)
		}
	}
}

// treePaths is the set of file paths a project's tree lists.
func treePaths(t *testing.T, h http.Handler, id string) map[string]bool {
	t.Helper()
	rec := do(t, h, "GET", "/api/p/"+id+"/tree", nil)
	if rec.Code != 200 {
		t.Fatalf("tree: %d %s", rec.Code, rec.Body)
	}
	var root Node
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	var walk func(*Node)
	walk = func(n *Node) {
		if !n.Dir && n.Path != "" {
			out[n.Path] = true
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(&root)
	return out
}
