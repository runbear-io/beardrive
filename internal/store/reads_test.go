package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadSpool(t *testing.T) {
	s := openTestStore(t)

	// Nothing queued: no batch, no error.
	if evs, err := s.PendingReads(); err != nil || len(evs) != 0 {
		t.Fatalf("empty spool = %v, %v", evs, err)
	}

	// Repeat reads of one path dedupe to its latest event.
	for i := 0; i < 3; i++ {
		if err := s.LogRead("wiki/a.md"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.LogRead("b.md"); err != nil {
		t.Fatal(err)
	}
	evs, err := s.PendingReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Path != "wiki/a.md" || evs[1].Path != "b.md" {
		t.Fatalf("batch = %+v, want deduped a.md + b.md", evs)
	}
	if evs[0].Time.IsZero() {
		t.Fatal("events must carry their read time")
	}

	// The batch survives until cleared — a failed report just retries — and
	// reads logged meanwhile land in a fresh spool behind it.
	if err := s.LogRead("c.md"); err != nil {
		t.Fatal(err)
	}
	again, err := s.PendingReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0].Path != "wiki/a.md" {
		t.Fatalf("retry batch = %+v, want the same uncleared batch", again)
	}
	if err := s.ClearPendingReads(); err != nil {
		t.Fatal(err)
	}
	next, err := s.PendingReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].Path != "c.md" {
		t.Fatalf("post-clear batch = %+v, want just c.md", next)
	}
	s.ClearPendingReads()
	if evs, _ := s.PendingReads(); len(evs) != 0 {
		t.Fatalf("drained spool still returned %+v", evs)
	}
}

func TestReadSpoolSurvivesCorruptLines(t *testing.T) {
	s := openTestStore(t)
	s.LogRead("good.md")
	f, err := os.OpenFile(s.readSpoolPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"path": "torn`) // a torn write
	f.Close()
	s.LogRead("also-good.md")
	evs, err := s.PendingReads()
	if err != nil {
		t.Fatal(err)
	}
	// The torn line joins the next event's line; both are dropped, but the
	// batch itself survives.
	if len(evs) == 0 || evs[0].Path != "good.md" {
		t.Fatalf("batch = %+v, want good.md to survive the torn line", evs)
	}
}

func TestReadSpoolCap(t *testing.T) {
	s := openTestStore(t)
	long := strings.Repeat("d", 1024)
	for i := 0; i < 1100; i++ { // ~1.1 MB of events
		if err := s.LogRead(long + "/" + string(rune('a'+i%26)) + ".md"); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(s.readSpoolPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > readSpoolMax+4096 {
		t.Fatalf("spool grew past its cap: %d bytes", fi.Size())
	}
}
