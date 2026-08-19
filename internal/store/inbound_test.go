package store

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestInboundSpool(t *testing.T) {
	s := openTestStore(t)

	// Nothing queued: no batch, no error.
	if evs, err := s.DrainInbound(); err != nil || len(evs) != 0 {
		t.Fatalf("empty spool = %v, %v", evs, err)
	}

	if err := s.LogInbound(InboundEvent{Path: "wiki/a.md", Deleted: false, Time: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := s.LogInbound(InboundEvent{Path: "b.md", Deleted: false, Time: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// A path written and then removed reports as deleted: latest wins.
	if err := s.LogInbound(InboundEvent{Path: "b.md", Deleted: true, Time: time.Now().UTC()}); err != nil {
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
	s.LogInbound(InboundEvent{Path: "good.md", Deleted: false, Time: time.Now().UTC()})
	f, err := os.OpenFile(s.inboundSpoolPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"path": "torn`) // a torn write
	f.Close()
	s.LogInbound(InboundEvent{Path: "also-good.md", Deleted: false, Time: time.Now().UTC()})
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
		if err := s.LogInbound(InboundEvent{Path: long + "/" + string(rune('a'+i%26)) + ".md", Deleted: false, Time: time.Now().UTC()}); err != nil {
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
	if err := s.LogInbound(InboundEvent{Path: "a.md", Deleted: false, Time: time.Now().UTC()}); err != nil {
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
	if err := s.LogInbound(InboundEvent{Path: "a.md", Deleted: false, Time: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.DrainInbound()
	if err != nil || len(evs) != 1 || evs[0].Path != "a.md" {
		t.Fatalf("drain after failure = %+v, %v, want a.md", evs, err)
	}
}
