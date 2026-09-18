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
