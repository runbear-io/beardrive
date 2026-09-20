package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A plain-folder server that accepts writes. dirServer is read-only, and the
// whole subject here is what happens on the write path.
func writableDirServer(t *testing.T, files map[string]string) http.Handler {
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
	s := &Server{
		Source: &DirSource{Root: root}, Volume: "local", Refresh: 0,
		Upload: UploadConfig{Enabled: true},
	}
	return s.Handler()
}

func putContent(t *testing.T, h http.Handler, url, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", url, strings.NewReader(body))
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	h.ServeHTTP(rec, r)
	return rec
}

/* A save that was based on a version somebody else has already replaced must
   not be taken wholesale.

   This is the door that lost real work: two browsers holding different
   documents — one had dropped the co-editing relay — each overwriting the
   other every few seconds, with no conflict copy and no warning. The sync
   path has preserved the loser since the beginning (syncer.conflictCopies);
   this one had nothing. */
func TestUploadRefusesAWriteBuiltOnAStaleVersion(t *testing.T) {
	h := writableDirServer(t, map[string]string{"notes.md": "base\n"})
	const url = "/api/upload/content?path=notes.md"

	first := putContent(t, h, url, "mine\n", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first write = %d: %s", first.Code, first.Body.String())
	}
	var out struct{ SHA string }
	if err := json.Unmarshal(first.Body.Bytes(), &out); err != nil || out.SHA == "" {
		t.Fatalf("write did not report the sha it produced: %s", first.Body.String())
	}

	// Somebody else writes, so the head moves off the sha we are holding.
	if rec := putContent(t, h, url, "theirs\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("second write = %d", rec.Code)
	}

	stale := putContent(t, h, url, "mine, extended\n", out.SHA)
	if stale.Code != http.StatusConflict {
		t.Fatalf("a write based on a replaced version = %d, want 409", stale.Code)
	}
	var conflict struct{ SHA string }
	if err := json.Unmarshal(stale.Body.Bytes(), &conflict); err != nil || conflict.SHA == "" {
		t.Fatalf("the 409 must carry the current sha, got: %s", stale.Body.String())
	}
	if conflict.SHA == out.SHA {
		t.Fatal("the 409 reported the sha we sent, not the one that replaced it")
	}

	// Rebased on what is actually there now, the same write is fine.
	if rec := putContent(t, h, url, "mine, extended\n", conflict.SHA); rec.Code != http.StatusOK {
		t.Fatalf("rebased write = %d: %s", rec.Code, rec.Body.String())
	}
}

// Every other writer on this route — older clients, MCP tools, anything that
// does not send the header — keeps working exactly as before.
func TestUploadWithoutIfMatchIsUnchanged(t *testing.T) {
	h := writableDirServer(t, map[string]string{"notes.md": "base\n"})
	const url = "/api/upload/content?path=notes.md"
	for _, body := range []string{"one\n", "two\n", "three\n"} {
		if rec := putContent(t, h, url, body, ""); rec.Code != http.StatusOK {
			t.Fatalf("unconditional write %q = %d", body, rec.Code)
		}
	}
}

/* Saving the same bytes twice is not two versions.

   Every co-editor after the first saves the same converged text, and a client
   whose PUT is queued behind a slow response re-sends on its next idle timer.
   Each used to append an op — a History row, a change frame, and a full tree
   refetch for every other client in the project.

   Against a real hub, not the folder viewer: a DirSource identifies a file by
   mtime and size and has no journal to keep clean, so this is a property of
   the thing that has one. */
func TestUploadOfIdenticalContentIsNotAChange(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	body := func(rec *httptest.ResponseRecorder) (sha string, unchanged bool) {
		t.Helper()
		var out struct {
			SHA       string `json:"sha"`
			Unchanged bool   `json:"unchanged"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%d %s: %v", rec.Code, rec.Body.String(), err)
		}
		return out.SHA, out.Unchanged
	}
	versions := func() int {
		t.Helper()
		rec := get(t, h, "/api/p/"+p.ID+"/history?path=notes.md&n=50")
		var out struct {
			Entries []struct{ Path string } `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
		}
		return len(out.Entries)
	}

	first := putContent(t, h, url, "the converged text\n", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first write = %d: %s", first.Code, first.Body.String())
	}
	sha1, unchanged := body(first)
	if unchanged {
		t.Fatal("the first write of new content reported itself unchanged")
	}
	if n := versions(); n != 1 {
		t.Fatalf("versions after one write = %d, want 1", n)
	}

	// The same bytes again: what every other co-editor sends.
	second := putContent(t, h, url, "the converged text\n", "")
	if second.Code != http.StatusOK {
		t.Fatalf("second write = %d: %s", second.Code, second.Body.String())
	}
	sha2, unchanged := body(second)
	if !unchanged {
		t.Error("re-saving identical bytes was taken as a change")
	}
	if sha2 != sha1 {
		t.Errorf("sha moved on a write that changed nothing: %s -> %s", sha1, sha2)
	}
	if n := versions(); n != 1 {
		t.Errorf("versions after re-saving the same bytes = %d, want 1", n)
	}

	// A real edit is still a real edit.
	third := putContent(t, h, url, "actually different\n", "")
	if _, unchanged := body(third); unchanged {
		t.Error("a real edit was suppressed as a no-op")
	}
	if n := versions(); n != 2 {
		t.Errorf("versions after a real edit = %d, want 2", n)
	}
}
