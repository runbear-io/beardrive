package webapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
