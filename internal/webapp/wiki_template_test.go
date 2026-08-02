package webapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The LLM wiki template is the one with real workflow rules in it, so prove
// the whole thing actually lands on a device and reaches the hub — including
// the two index files the pattern depends on.
func TestCLIWikiTemplate(t *testing.T) {
	e := newCLIEnv(t)
	run, hub, browser := e.run, e.hub, e.browser

	work := filepath.Join(t.TempDir(), "brain")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	defer run(work, "stop", work)
	if out, err := run(work, "init", "--name", "llmwiki", "--template", "wiki", "--yes"); err != nil {
		t.Fatalf("init --template wiki: %v\n%s", err, out)
	}
	want := []string{"AGENTS.md", "index.md", "log.md", "sources/README.md", "wiki/README.md"}
	paths := hubPaths(t, browser, hub.URL, projectIDByName(t, browser, hub.URL, "llmwiki"))
	for _, rel := range want {
		if !fileExists(filepath.Join(work, filepath.FromSlash(rel))) {
			t.Errorf("%s missing on disk", rel)
		}
		if !paths[rel] {
			t.Errorf("%s never reached the hub: %v", rel, paths)
		}
	}
	// The index rule is the load-bearing instruction; if it ever drops out of
	// the AGENTS.md the pattern quietly stops working.
	agents, err := os.ReadFile(filepath.Join(work, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents), "incomplete write") {
		t.Error("AGENTS.md lost the index-update-is-part-of-the-write rule")
	}
}
