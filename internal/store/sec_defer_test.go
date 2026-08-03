package store

// Round 5, from the completeness sweep: the exported surface of this package
// that no TestSec_* names. LogRead / PendingReads / ClearPendingReads and
// PutBlobBytes / PutBlobFile / PutBlobReader were on that list; the read spool
// is the one that turned out to matter.
//
// Round 4 made the volume's own state 0600 because "every local account could
// read a private project's path list, authorship and signed-in emails"
// (TestSec_Store_VolumeJournalsAreNotWorldReadable). It changed
// WriteFileAtomic's callers. The read spool is written by a different call —
// os.OpenFile(..., 0o644) in reads.go — and holds exactly the same class of
// data: every path an agent read inside the project, timestamped.
//
// Helper prefix: secdef.

import (
	"os"
	"path/filepath"
	"testing"
)

// secdefModes lists the mode of every regular file the store created under
// its directory, so a new state file cannot be added at 0644 unnoticed.
func secdefModes(t *testing.T, dir string) map[string]os.FileMode {
	t.Helper()
	out := map[string]os.FileMode{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out[rel] = fi.Mode().Perm()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSec_Store_ReadSpoolIsNotWorldReadable: the agent read log is a list of
// the files a person or agent opened inside a project — which files exist,
// which ones matter, and when they were touched. On a shared machine that is
// exactly what round 4 decided the journal must not expose, and the spool sits
// beside it in the same 0755 volume directory at mode 0644.
func TestSec_Store_ReadSpoolIsNotWorldReadable(t *testing.T) {
	s, _ := secpkgStore(t)
	if err := s.LogRead("secret-project/acquisition-plan.md"); err != nil {
		t.Fatal(err)
	}
	found := false
	for rel, mode := range secdefModes(t, s.Dir()) {
		if mode&0o077 == 0 {
			continue
		}
		// The flock file is empty by design and carries nothing.
		if rel == "lock" {
			continue
		}
		found = true
		t.Errorf("%s is mode %04o — every local account can read it; "+
			"the read spool names the files this project's agent opened", rel, mode)
	}
	if !found {
		// Prove the spool was actually written, so a green run means the
		// guard held rather than that nothing was created.
		if len(secdefModes(t, s.Dir())) == 0 {
			t.Fatal("fixture wrong: LogRead created no file to check")
		}
	}
}

// TestSec_Store_ReadSpoolSurvivesAHostilePathAsData is the injection question
// for the same file: the paths come from an agent hook firing on every Read,
// Grep and Bash, so they are whatever a filename in a synced project says.
// A newline or a brace must stay inside one JSON record and must not forge a
// second read event.
func TestSec_Store_ReadSpoolSurvivesAHostilePathAsData(t *testing.T) {
	s, _ := secpkgStore(t)
	hostile := []string{
		"notes/real.md",
		"a\n{\"path\":\"forged/by-newline.md\",\"time\":\"2030-01-01T00:00:00Z\"}\nb.md",
		"quote\"break.md",
		"brace}{.md",
		"tab\there.md",
	}
	for _, p := range hostile {
		if err := s.LogRead(p); err != nil {
			t.Fatalf("LogRead(%q): %v", p, err)
		}
	}
	got, err := s.PendingReads()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Path] = true
	}
	for _, p := range hostile {
		if !seen[p] {
			t.Errorf("LogRead(%q) did not round-trip through the spool: %+v", p, got)
		}
	}
	if seen["forged/by-newline.md"] {
		t.Error("a newline inside one path forged a second read event in the spool: " +
			"the spool is line-delimited and the path is not encoded as data")
	}
	if len(got) != len(hostile) {
		t.Errorf("spool holds %d events for %d logged reads: %+v", len(got), len(hostile), got)
	}
}
