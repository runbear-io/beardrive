package docrefs

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Resolution is the filter: a candidate that does not land on a synced file is
// not a reference, however path-shaped it looks.
func TestResolve(t *testing.T) {
	synced := map[string]bool{
		"internal/syncer/syncer.go": true,
		"docs/hub-config.md":        true,
		"docs/nested/deep.go":       true,
		"README.md":                 true,
	}
	exists := func(p string) bool { return synced[p] }
	cases := []struct {
		name, doc, cand, want string
	}{
		{"inline link, root-relative", "a.md", "internal/syncer/syncer.go", "internal/syncer/syncer.go"},
		{"relative to the doc's own dir", "docs/nested/a.md", "deep.go", "docs/nested/deep.go"},
		{"falls back to the root", "docs/a.md", "internal/syncer/syncer.go", "internal/syncer/syncer.go"},
		{"wikilink retried with .md", "a.md", "docs/hub-config.md", "docs/hub-config.md"},
		{"anchor stripped", "a.md", "README.md#install", "README.md"},
		{"query stripped", "a.md", "README.md?raw=1", "README.md"},
		{"trailing sentence period", "a.md", "README.md.", "README.md"},
		{"dot-slash prefix", "a.md", "./README.md", "README.md"},
		{"http url", "a.md", "https://example.com/README.md", ""},
		{"mailto", "a.md", "mailto:someone@example.com", ""},
		{"protocol-relative", "a.md", "//example.com/README.md", ""},
		{"absolute path", "a.md", "/etc/passwd", ""},
		{"escapes the mount", "docs/a.md", "../../../etc/passwd", ""},
		{"made up", "a.md", "internal/nope/missing.go", ""},
		{"empty", "a.md", "", ""},
		{"self-link", "README.md", "README.md", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.doc, []string{tc.cand}, exists)
			var want []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", tc.doc, tc.cand, got, want)
			}
		})
	}
}

// The three extractors, in document order, and a wikilink tried both ways.
func TestCandidatesAndDedupe(t *testing.T) {
	doc := "see [cfg](deploy/config.yaml) and [[guide]]\nbare: `src/main.go`, again deploy/config.yaml\n"
	cands := Candidates(strings.NewReader(doc))
	exists := func(p string) bool {
		return p == "deploy/config.yaml" || p == "guide.md" || p == "src/main.go"
	}
	got := Resolve("runbook.md", cands, exists)
	want := []string{"deploy/config.yaml", "guide.md", "src/main.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A peer stamping a put in the future cannot date a path with it; a delete
// never dates one; the newest real write wins.
func TestWriteTimesClampsFutureStamps(t *testing.T) {
	now := time.Now()
	ops := []journal.Op{
		{Kind: journal.KindPut, Path: "a", Time: now.Add(-2 * time.Hour)},
		{Kind: journal.KindPut, Path: "a", Time: now.Add(-1 * time.Hour)},
		{Kind: journal.KindDelete, Path: "a", Time: now.Add(-time.Minute)},
		{Kind: journal.KindPut, Path: "future", Time: now.Add(24 * time.Hour)},
	}
	w := WriteTimes(ops)
	if !w["a"].Equal(now.Add(-1 * time.Hour)) {
		t.Errorf("a dated %v", w["a"])
	}
	if _, ok := w["future"]; ok {
		t.Errorf("a future stamp dated a path: %v", w["future"])
	}
}

func TestOutgrownWorstFirst(t *testing.T) {
	doc := time.Unix(1000, 0)
	times := map[string]time.Time{
		"older": time.Unix(500, 0),
		"near":  time.Unix(1100, 0),
		"far":   time.Unix(9000, 0),
	}
	got := Outgrown(doc, []string{"older", "near", "far", "undated"}, func(p string) (time.Time, bool) {
		t, ok := times[p]
		return t, ok
	})
	if len(got) != 2 || got[0].Path != "far" || got[1].Path != "near" {
		t.Fatalf("got %+v", got)
	}
	if got[1].Gap != 100*time.Second {
		t.Errorf("gap %v", got[1].Gap)
	}
}
