package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

// The reported bug was read off `bdrive log`'s own output, so assert on that
// output: every printed timestamp descends, the stamp is the file's edit time
// (files written minutes apart don't collapse onto one), and -n keeps the
// newest by that stamp rather than the highest lamport.
func TestLogPrintsNewestFirstByEditTime(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678", // unreachable: the cycle degrades offline
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.ResolveMount(folder); err != nil { // enroll, as `bdrive init` would
		t.Fatal(err)
	}

	// Written in one scan, edited at three different times — and the newest
	// edit is deliberately not the one the walk sees last.
	now := time.Now()
	files := map[string]time.Duration{
		"a-oldest.md": -30 * time.Minute,
		"b-newest.md": -1 * time.Minute,
		"c-middle.md": -10 * time.Minute,
	}
	for rel, age := range files {
		abs := filepath.Join(folder, rel)
		if err := os.WriteFile(abs, []byte("content of "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
		when := now.Add(age)
		if err := os.Chtimes(abs, when, when); err != nil {
			t.Fatal(err)
		}
	}

	sync := syncCmd()
	sync.SetOut(&bytes.Buffer{})
	sync.SetArgs([]string{folder})
	if err := sync.Execute(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	stamps, paths := runLog(t, folder)
	if len(paths) != 3 {
		t.Fatalf("got %d rows, want 3:\n%v", len(paths), paths)
	}
	if want := []string{"b-newest.md", "c-middle.md", "a-oldest.md"}; !equal(paths, want) {
		t.Fatalf("row order = %v, want %v", paths, want)
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i].After(stamps[i-1]) {
			t.Fatalf("printed stamps are not newest-first: %v then %v", stamps[i-1], stamps[i])
		}
	}
	// Three edits minutes apart must print three distinct stamps; before this
	// change one scan stamped them all with its own commit time.
	if stamps[0].Equal(stamps[1]) || stamps[1].Equal(stamps[2]) {
		t.Fatalf("stamps collapsed onto the sync time: %v", stamps)
	}

	// -n truncates after the display sort.
	_, top := runLog(t, folder, "-n", "2")
	if want := []string{"b-newest.md", "c-middle.md"}; !equal(top, want) {
		t.Fatalf("-n 2 = %v, want %v", top, want)
	}

	// -p still filters to one path.
	_, only := runLog(t, folder, "-p", "c-middle.md")
	if want := []string{"c-middle.md"}; !equal(only, want) {
		t.Fatalf("-p = %v, want %v", only, want)
	}
}

// runLog runs `bdrive log` and parses the printed timestamp + path columns.
func runLog(t *testing.T, folder string, extra ...string) ([]time.Time, []string) {
	t.Helper()
	c := logCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs(append([]string{folder}, extra...))
	if err := c.Execute(); err != nil {
		t.Fatalf("log: %v", err)
	}
	var stamps []time.Time
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local)
		if err != nil {
			t.Fatalf("unparsable log row %q: %v", line, err)
		}
		fields := strings.Fields(line[19:])
		stamps = append(stamps, ts)
		paths = append(paths, fields[1])
	}
	return stamps, paths
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
