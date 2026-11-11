package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func putSHA(t *testing.T, h http.Handler, url, body, ifMatch string) string {
	t.Helper()
	rec := putContent(t, h, url, body, ifMatch)
	if rec.Code/100 != 2 {
		t.Fatalf("PUT %q (If-Match %q): %d %s", body, ifMatch, rec.Code, rec.Body.String())
	}
	var out struct{ SHA string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.SHA == "" {
		t.Fatalf("PUT %q: no sha in %s", body, rec.Body.String())
	}
	return out.SHA
}

/*
The history X -> A -> X -> B holds a put of A after a put of X, but a stale
write of A built on the SECOND X is not a retry of the first: it never landed
on that X. It must be merged with B, not answered with B unchanged.
*/
func TestUploadStaleWriteOnRevertedBaseMergesInsteadOfRetry(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	u := "/api/p/" + p.ID + "/upload/content?path=abab.md"
	x := "a\nb\nc\nd\ne\n"
	a := "a\nA\nc\nd\ne\n"
	b := "a\nb\nc\nd\nB\n"

	x1 := putSHA(t, h, u, x, "")
	a1 := putSHA(t, h, u, a, x1)
	x2 := putSHA(t, h, u, x, a1)
	if x2 != x1 {
		t.Fatalf("same bytes, different sha: %s vs %s", x1, x2)
	}
	putSHA(t, h, u, b, x2)

	rec := putContent(t, h, u, a, x2)
	if rec.Code/100 != 2 {
		t.Fatalf("stale PUT A: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Merged    bool
		Unchanged bool
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Unchanged {
		t.Fatalf("stale write of A on the reverted X was treated as a landed retry: %s", rec.Body.String())
	}
	got := get(t, h, "/api/p/"+p.ID+"/file?path=abab.md").Body.String()
	if want := "a\nA\nc\nd\nB\n"; got != want {
		t.Fatalf("file = %q, want A merged with B %q", got, want)
	}
}

/*
T5: Alice's write lands on V0, Bob's lands on top of it (merged), then
Alice's client retries her exact request after a lost response. The retry is
recognised on the head's Base chain and her line appears exactly once.
*/
func TestUploadRetryOfLandedWriteIsIdempotent(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	u := "/api/p/" + p.ID + "/upload/content?path=t5.md"
	base := "# List\n\n- item 1\n"
	alice := "# List\n\n- item 1\n- alice\n"
	bob := "# List\n\n- item 1\n- bob\n"

	v0 := putSHA(t, h, u, base, "")
	putSHA(t, h, u, alice, v0)
	if rec := putContent(t, h, u, bob, v0); rec.Code/100 != 2 && rec.Code != http.StatusConflict {
		t.Fatalf("bob: %d %s", rec.Code, rec.Body.String())
	}
	rec := putContent(t, h, u, alice, v0)
	if rec.Code/100 != 2 {
		t.Fatalf("alice retry: %d %s", rec.Code, rec.Body.String())
	}
	got := get(t, h, "/api/p/"+p.ID+"/file?path=t5.md").Body.String()
	if n := strings.Count(got, "- alice\n"); n != 1 {
		t.Fatalf("alice appears %d times in %q", n, got)
	}
}

/*
X -> A -> B where B removed A's line: a resubmission of A built on X is on
A's lineage, but the head no longer carries A's edit. Answering B unchanged
would drop the user's write; it must be merged again (or refused).
*/
func TestUploadResubmitAfterHeadRemovedItIsNotDiscarded(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	u := "/api/p/" + p.ID + "/upload/content?path=xab.md"
	x := "a\nc\n"
	a := "a\nA\nc\n"
	b := "a\nc\nB\n"

	x1 := putSHA(t, h, u, x, "")
	a1 := putSHA(t, h, u, a, x1)
	b1 := putSHA(t, h, u, b, a1)

	rec := putContent(t, h, u, a, x1)
	if rec.Code == http.StatusConflict {
		return // refused, never silently discarded
	}
	if rec.Code/100 != 2 {
		t.Fatalf("resubmit A: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		SHA       string
		Unchanged bool
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Unchanged || out.SHA == b1 {
		t.Fatalf("resubmitted A was answered with B unchanged: %s", rec.Body.String())
	}
	got := get(t, h, "/api/p/"+p.ID+"/file?path=xab.md").Body.String()
	if want := "a\nA\nc\nB\n"; got != want {
		t.Fatalf("file = %q, want A merged onto B %q", got, want)
	}
}

/*
Two edits that each keep config.json valid — one adds "a", the other "b",
both at the same point of an empty object — merge line-wise into a document
with no comma between the keys. The hub must refuse that merge (409) rather
than commit JSON no reader can parse, matching the device-side merge.
*/
func TestUploadMergeRejectsInvalidCombinedJSON(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	u := "/api/p/" + p.ID + "/upload/content?path=config.json"
	base := putSHA(t, h, u, "{\n}\n", "")
	putSHA(t, h, u, "{\n\"a\": 1\n}\n", base)

	rec := putContent(t, h, u, "{\n\"b\": 2\n}\n", base)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale PUT whose merge is invalid JSON: %d %s, want 409", rec.Code, rec.Body.String())
	}
	got := get(t, h, "/api/p/"+p.ID+"/file?path=config.json").Body.String()
	if !json.Valid([]byte(got)) || !strings.Contains(got, `"a": 1`) || strings.Contains(got, `"b"`) {
		t.Fatalf("file = %q, want the landed {\"a\": 1} left untouched", got)
	}
}

/*
A closing tab's last save that clashes with the file is kept as a conflict copy
by the hub, in that one request: the page is gone before it could answer a 409
with a second PUT. Asked for with X-Bdrive-Park-On-Conflict, and only ever as a
last resort — a stale save that merges cleanly still merges.
*/
func TestUploadContentParkOnConflictWritesCopyInOneRequest(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	const path = "notes.md"
	url := "/api/p/" + p.ID + "/upload/content?path=" + path
	file := "/api/p/" + p.ID + "/file?path=" + path
	const base = "l1\nl2\nl3\n"
	const head = "l1 from somebody else\nl2\nl3\n"
	baseSha := putSHA(t, h, url, base, "")
	putSHA(t, h, url, head, baseSha)

	parkPut := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", url, strings.NewReader(body))
		r.Header.Set("If-Match", baseSha)
		r.Header.Set(parkOnConflictHeader, "1")
		h.ServeHTTP(rec, r)
		return rec
	}
	type answer struct {
		OK     bool   `json:"ok"`
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
		Parked string `json:"parked"`
	}

	const mine = "l1 from the closing tab\nl2\nl3\n"
	rec := parkPut(mine)
	if rec.Code != http.StatusOK {
		t.Fatalf("clashing save: %d %s", rec.Code, rec.Body)
	}
	var out answer
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Merged || !strings.HasPrefix(out.Parked, path+".bdrive-conflict-") {
		t.Fatalf("answer %s: want ok naming a conflict copy", rec.Body)
	}
	if out.SHA != shaOf(head) {
		t.Fatalf("answered sha %s, want the head the file stays on", out.SHA)
	}
	if got := get(t, h, file).Body.String(); got != head {
		t.Fatalf("file = %q, want the head %q", got, head)
	}
	if got := get(t, h, "/api/p/"+p.ID+"/file?path="+out.Parked).Body.String(); got != mine {
		t.Fatalf("copy = %q, want the caller's %q", got, mine)
	}

	// Different lines: merged onto the file, nothing parked.
	rec = parkPut("l1\nl2\nl3 from the closing tab\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("mergeable save: %d %s", rec.Code, rec.Body)
	}
	out = answer{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Merged || out.Parked != "" {
		t.Fatalf("mergeable save answered %s: want merged, nothing parked", rec.Body)
	}
	if got, want := get(t, h, file).Body.String(), "l1 from somebody else\nl2\nl3 from the closing tab\n"; got != want {
		t.Fatalf("file = %q, want the merge %q", got, want)
	}
	if cc := conflictTexts(t, srv, p.ID, path); len(cc) != 1 {
		t.Fatalf("%d conflict copies, want only the clash's", len(cc))
	}
}

// conflictStamp checks name is a conflict copy of path in the form the sync
// engine recognises — <path>.bdrive-conflict-<who>-<UTC second> — and
// returns its second.
func conflictStamp(t *testing.T, path, name string) time.Time {
	t.Helper()
	rest, ok := strings.CutPrefix(name, path+".bdrive-conflict-")
	i := strings.LastIndex(rest, "-")
	if !ok || i <= 0 {
		t.Fatalf("%q is not a conflict copy of %s", name, path)
	}
	at, err := time.Parse("20060102T150405Z", rest[i+1:])
	if err != nil {
		t.Fatalf("%q: %v", name, err)
	}
	return at
}

// parkAs is a closing tab's save: conditional on base, asking the hub to
// park it if it clashes. It answers the copy's name.
func parkAs(t *testing.T, h http.Handler, url, base, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", url, strings.NewReader(body))
	r.Header.Set("If-Match", base)
	r.Header.Set(parkOnConflictHeader, "1")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("park %q: %d %s", body, rec.Code, rec.Body)
	}
	var out struct {
		Parked string `json:"parked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Parked == "" {
		t.Fatalf("park %q answered %s: no copy named", body, rec.Body)
	}
	return out.Parked
}

/*
Conflict copies are named to the second, and two tabs of one account can close
in the same one. Each copy is created only under a name nobody holds: the
second tab's moves a second on instead of replacing the first's, and a name
already taken — here, the next few seconds' copies of somebody who parked
before — is never written over either.
*/
func TestUploadParkOnConflictNeverReplacesACopyFromTheSameSecond(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	const path = "notes.md"
	url := "/api/p/" + p.ID + "/upload/content?path=" + path
	baseSha := putSHA(t, h, url, "l1\nl2\nl3\n", "")
	putSHA(t, h, url, "l1 from somebody else\nl2\nl3\n", baseSha)

	// Every copy here is the anonymous caller's, so the names are shared.
	now := time.Now()
	squatters := map[string]string{}
	for k := range 3 {
		name := conflictPath(path, User{}, now.Add(time.Duration(k)*time.Second))
		text := fmt.Sprintf("an earlier copy %d\n", k)
		putSHA(t, h, "/api/p/"+p.ID+"/upload/content?path="+name, text, "")
		squatters[name] = text
	}

	const tab1, tab2 = "l1 from tab one\nl2\nl3\n", "l1 from tab two\nl2\nl3\n"
	first := parkAs(t, h, url, baseSha, tab1)
	second := parkAs(t, h, url, baseSha, tab2)
	if first == second {
		t.Fatalf("both tabs parked at %s", first)
	}
	a, b := conflictStamp(t, path, first), conflictStamp(t, path, second)
	if !b.After(a) {
		t.Fatalf("second tab's copy %s is not after the first's %s", second, first)
	}
	cc := conflictTexts(t, srv, p.ID, path)
	for name, text := range squatters {
		if cc[name] != text {
			t.Fatalf("%s = %q, want the copy that was already there", name, cc[name])
		}
	}
	if cc[first] != tab1 || cc[second] != tab2 {
		t.Fatalf("copies %v: want tab one's at %s and tab two's at %s", cc, first, second)
	}
	if len(cc) != len(squatters)+2 {
		t.Fatalf("%d conflict copies, want %d", len(cc), len(squatters)+2)
	}
}

/*
The same across two hub processes: the second process may not have folded the
first's copy yet, so a free-looking name is held to absent in the journal
append itself, where the other process's write is guaranteed to be seen.
*/
func TestUploadParkOnConflictAcrossProcessesNeverReplacesACopy(t *testing.T) {
	a, b, p := newHubPair(t)
	ha, hb := a.Handler(), b.Handler()
	const path = "notes.md"
	url := "/api/p/" + p.ID + "/upload/content?path=" + path
	baseSha := putSHA(t, ha, url, "l1\nl2\nl3\n", "")
	putSHA(t, ha, url, "l1 from somebody else\nl2\nl3\n", baseSha)
	// B reads before A parks, so its snapshot predates A's copy.
	if rec := get(t, hb, "/api/p/"+p.ID+"/tree"); rec.Code != http.StatusOK {
		t.Fatalf("tree on B: %d", rec.Code)
	}

	const tab1, tab2 = "l1 from tab one on A\nl2\nl3\n", "l1 from tab two on B\nl2\nl3\n"
	first := parkAs(t, ha, url, baseSha, tab1)
	second := parkAs(t, hb, url, baseSha, tab2)
	if first == second {
		t.Fatalf("both processes parked at %s", first)
	}
	conflictStamp(t, path, first)
	conflictStamp(t, path, second)
	for _, s := range []*Server{a, b} {
		cc := conflictTexts(t, s, p.ID, path)
		if len(cc) != 2 || cc[first] != tab1 || cc[second] != tab2 {
			t.Fatalf("copies on %s: %v, want A's at %s and B's at %s", s.Device.ID, cc, first, second)
		}
	}
}
