package webapp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole review loop through the real binary: a person comments on a
// rendered quote in the browser, the agent lists open comments and finds the
// source line the quote is on, then resolves the thread with a note.
func TestCLICommentsSweep(t *testing.T) {
	e := newCLIEnv(t)
	run := e.run

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "# Q3 report\n\nIntro paragraph.\n\nRevenue grew by **12%** this quarter.\n"
	if err := os.WriteFile(filepath.Join(work, "wiki", "report.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(work, "init", "--name", "review", "--yes"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	defer run(work, "stop", work)
	if out, err := run(work, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	id := projectIDByName(t, e.browser, e.hub.URL, "review")

	// What the viewer sends: the RENDERED text — no ** — with its context.
	body, _ := json.Marshal(map[string]any{
		"path": "wiki/report.md",
		"body": "Finance says 14%.",
		"anchor": map[string]string{
			"quote": "grew by 12% this", "prefix": "Revenue ", "suffix": " quarter.",
		},
	})
	resp, err := e.browser.Post(e.hub.URL+"/api/p/"+id+"/comments", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("browser comment: %d", resp.StatusCode)
	}

	out, err := run(filepath.Join(work, "wiki"), "comments", "--json")
	if err != nil {
		t.Fatalf("comments --json: %v\n%s", err, out)
	}
	var list []struct {
		ID       string `json:"id"`
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Located  bool   `json:"located"`
		URL      string `json:"url"`
		Messages []struct {
			Body string `json:"body"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("comments --json is not JSON: %v\n%s", err, out)
	}
	if len(list) != 1 || list[0].Path != "wiki/report.md" || list[0].Line != 5 || !list[0].Located {
		t.Fatalf("listing = %+v", list)
	}
	if list[0].Messages[0].Body != "Finance says 14%." || !strings.Contains(list[0].URL, "?comment="+list[0].ID) {
		t.Fatalf("listing = %+v", list)
	}

	text, err := run(work, "comments", "wiki")
	if err != nil || !strings.Contains(text, "wiki/report.md:5") || !strings.Contains(text, "Finance says 14%.") {
		t.Fatalf("text listing: %v\n%s", err, text)
	}

	if out, err := run(work, "comments", "resolve", list[0].ID[:5], "-m", "Changed to 14%"); err != nil || !strings.Contains(out, "resolved "+list[0].ID) {
		t.Fatalf("resolve: %v\n%s", err, out)
	}
	if out, _ := run(work, "comments"); !strings.Contains(out, "No open comments.") {
		t.Fatalf("after resolve, open listing:\n%s", out)
	}
	if out, _ := run(work, "comments", "--all"); !strings.Contains(out, "resolved") || !strings.Contains(out, "Changed to 14%") {
		t.Fatalf("--all listing:\n%s", out)
	}
	if out, err := run(work, "comments", "reply", list[0].ID, "-m", ""); err == nil {
		t.Fatalf("an empty reply was sent:\n%s", out)
	}
}
