package webapp

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dirServer(t *testing.T, files map[string]string) http.Handler {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{Source: &DirSource{Root: root}, Remote: root, Volume: "local", Refresh: 0}
	return s.Handler()
}

func TestDirSourceServesFolder(t *testing.T) {
	h := dirServer(t, map[string]string{
		"README.md":     "# Local",
		"notes/plan.md": "content",
		".sfs":          `{"volume":"x"}`, // settings file must be hidden
		".git/config":   "noise",          // .git must be skipped
	})

	var root Node
	if err := json.Unmarshal(get(t, h, "/api/tree").Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, n := range root.Children {
		names = append(names, n.Name)
	}
	if len(root.Children) != 2 || root.Children[0].Name != "notes" || root.Children[1].Name != "README.md" {
		t.Fatalf("tree children = %v, want [notes README.md]", names)
	}

	rec := get(t, h, "/api/file?path=notes/plan.md")
	if rec.Code != 200 || rec.Body.String() != "content" {
		t.Fatalf("file: %d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("dir source should still produce ETags")
	}

	rec = get(t, h, "/api/render?path=README.md")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Local") {
		t.Fatalf("render: %d %s", rec.Code, rec.Body)
	}

	if rec := get(t, h, "/api/file?path=.git/config"); rec.Code != 404 {
		t.Fatalf(".git content must be hidden, got %d", rec.Code)
	}
	if rec := get(t, h, "/api/file?path=../escape"); rec.Code != 404 {
		t.Fatalf("path traversal must 404, got %d", rec.Code)
	}
}
