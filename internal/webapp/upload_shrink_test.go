package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func headSha(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct{ SHA string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.SHA == "" {
		t.Fatalf("%d %s: no sha", rec.Code, rec.Body.String())
	}
	return out.SHA
}

/* An editor that attached before its document had content saved 0 bytes over
   four meeting notes, each with the right If-Match. The base check cannot
   see that; this one does. */
func TestEditorSaveThatEmptiesAFileIsRefused(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	full := strings.Repeat("a standup line\n", 100)

	base := headSha(t, putContent(t, h, url, full, ""))

	empty := putContent(t, h, url, "", base)
	if empty.Code != http.StatusUnprocessableEntity {
		t.Fatalf("emptying save = %d, want 422: %s", empty.Code, empty.Body.String())
	}
	var refusal struct {
		Shrink bool
		SHA    string
		Size   int64
	}
	if err := json.Unmarshal(empty.Body.Bytes(), &refusal); err != nil || !refusal.Shrink || refusal.SHA != base || refusal.Size != int64(len(full)) {
		t.Fatalf("refusal body = %s", empty.Body.String())
	}
	if rec := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md"); rec.Body.String() != full {
		t.Fatalf("file changed after a refused save: %q", rec.Body.String())
	}

	shrunk := putContent(t, h, url, "a standup line\n", base)
	if shrunk.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save keeping under a fifth = %d, want 422", shrunk.Code)
	}

	// The user meant it.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", url, strings.NewReader(""))
	r.Header.Set("If-Match", base)
	r.Header.Set(confirmShrinkHeader, "1")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed emptying save = %d: %s", rec.Code, rec.Body.String())
	}
}

// Unconditional writes (uploads, MCP, older clients) replace files wholesale
// and are not editor saves.
func TestUnconditionalWriteMayEmptyAFile(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	putContent(t, h, url, strings.Repeat("x", 1000), "")
	if rec := putContent(t, h, url, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("unconditional empty write = %d", rec.Code)
	}
}

func TestSharpShrink(t *testing.T) {
	for _, c := range []struct {
		was, now int64
		want     bool
	}{
		{0, 0, false},
		{10, 0, true},
		{10, 1, false},
		{1000, 199, true},
		{1000, 200, false},
		{1000, 2000, false},
		{shrinkRatioFloor - 1, 1, false},
	} {
		if got := sharpShrink(c.was, c.now); got != c.want {
			t.Errorf("sharpShrink(%d, %d) = %v, want %v", c.was, c.now, got, c.want)
		}
	}
}
