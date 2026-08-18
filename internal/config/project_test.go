package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPostSyncRoundTrip: the hook command survives save/load, and stays out of
// the file entirely when unset.
func TestPostSyncRoundTrip(t *testing.T) {
	folder := t.TempDir()
	if _, err := SaveProject(folder, Project{ID: "m-1234abcd", Volume: "wiki", PostSync: "qmd update && qmd embed"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadProject(folder)
	if err != nil || !ok {
		t.Fatalf("LoadProject: %v (ok=%v)", err, ok)
	}
	if got.PostSync != "qmd update && qmd embed" {
		t.Fatalf("post_sync = %q, want the saved command", got.PostSync)
	}

	if _, err := SaveProject(folder, Project{ID: "m-1234abcd", Volume: "wiki"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(folder, ProjectDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "post_sync") {
		t.Fatalf("unset post_sync should be omitted, got %s", raw)
	}
}

func TestNormalizeInclude(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{in: nil, want: nil},
		{in: []string{"wiki/"}, want: []string{"/wiki/"}}, // legacy config, anchored on read
		{in: []string{"wiki"}, want: []string{"/wiki"}},
		{in: []string{"/wiki/"}, want: []string{"/wiki/"}}, // already anchored
		{in: []string{"a/b/"}, want: []string{"a/b/"}},     // compile() anchors these already
		{in: []string{"*.md"}, want: []string{"*.md"}},     // deliberate pattern, left alone
		{in: []string{"!keep"}, want: []string{"!keep"}},
		{in: []string{"wiki/", "*.md"}, want: []string{"/wiki/", "*.md"}},
	} {
		in := append([]string(nil), tc.in...)
		if got := normalizeInclude(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("normalizeInclude(%q) = %q, want %q", in, got, tc.want)
		}
	}
}
