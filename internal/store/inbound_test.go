package store

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestInboundSpool(t *testing.T) {
	s := openTestStore(t)

	// Nothing queued: no batch, no error.
	if evs, err := s.DrainInbound(); err != nil || len(evs) != 0 {
		t.Fatalf("empty spool = %v, %v", evs, err)
	}

	if err := s.LogInbound("wiki/a.md", false); err != nil {
		t.Fatal(err)
	}
	if err := s.LogInbound("b.md", false); err != nil {
		t.Fatal(err)
	}
	// A path written and then removed reports as deleted: latest wins.
	if err := s.LogInbound("b.md", true); err != nil {
		t.Fatal(err)
	}
	evs, err := s.DrainInbound()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Path != "wiki/a.md" || evs[1].Path != "b.md" {
		t.Fatalf("batch = %+v, want wiki/a.md + b.md", evs)
	}
	if evs[0].Deleted || !evs[1].Deleted {
		t.Fatalf("batch = %+v, want b.md marked deleted", evs)
	}
	if evs[0].Time.IsZero() {
		t.Fatal("events must carry their time")
	}

	// The drain clears: a second run with no activity in between reports
	// nothing.
	if again, err := s.DrainInbound(); err != nil || len(again) != 0 {
		t.Fatalf("second drain = %+v, %v, want empty", again, err)
	}
}

func TestInboundSpoolSurvivesCorruptLines(t *testing.T) {
	s := openTestStore(t)
	s.LogInbound("good.md", false)
	f, err := os.OpenFile(s.inboundSpoolPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"path": "torn`) // a torn write
	f.Close()
	s.LogInbound("also-good.md", false)
	evs, err := s.DrainInbound()
	if err != nil {
		t.Fatal(err)
	}
	// The torn line joins the next event's line; both are dropped, but the
	// batch itself survives.
	if len(evs) == 0 || evs[0].Path != "good.md" {
		t.Fatalf("batch = %+v, want good.md to survive the torn line", evs)
	}
}

func TestInboundSpoolCap(t *testing.T) {
	s := openTestStore(t)
	long := strings.Repeat("d", 1024)
	for i := 0; i < 1100; i++ { // ~1.1 MB of events
		if err := s.LogInbound(long+"/"+string(rune('a'+i%26))+".md", false); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(s.inboundSpoolPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > inboundSpoolMax+4096 {
		t.Fatalf("spool grew past its cap: %d bytes", fi.Size())
	}
}

// The spool is a plain file in the volume dir at 0600 — never in the working
// folder, never synced.
func TestInboundSpoolPermissions(t *testing.T) {
	s := openTestStore(t)
	if err := s.LogInbound("a.md", false); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.inboundSpoolPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("spool mode = %v, want 0600", fi.Mode().Perm())
	}
}

// An unreadable spool must not wedge every later drain behind it.
func TestInboundSpoolUnreadableRecovers(t *testing.T) {
	s := openTestStore(t)
	if err := os.MkdirAll(s.inboundSpoolPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainInbound(); err == nil {
		t.Fatal("unreadable spool should report its error")
	}
	if err := s.LogInbound("a.md", false); err != nil {
		t.Fatal(err)
	}
	evs, err := s.DrainInbound()
	if err != nil || len(evs) != 1 || evs[0].Path != "a.md" {
		t.Fatalf("drain after failure = %+v, %v, want a.md", evs, err)
	}
}

// A daemon appends while a hook drains, outside the volume lock. Every event
// must reach exactly one drain: an append into a spool the drain had already
// renamed and read was removed with the drain file.
func TestInboundDrainLosesNoConcurrentAppend(t *testing.T) {
	s := openTestStore(t)
	const writers, each = 4, 1000
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := s.LogInboundForMount("m", fmt.Sprintf("w%d/%d.md", w, i), false); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	seen := map[string]bool{}
	drain := func() {
		evs, err := s.DrainInboundForMount("m")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			seen[e.Path] = true
		}
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			drain()
		}
	}
	drain()
	if len(seen) != writers*each {
		t.Fatalf("drains saw %d of %d appended events", len(seen), writers*each)
	}
}
