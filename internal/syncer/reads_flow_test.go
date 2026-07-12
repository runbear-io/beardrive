package syncer

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// readReportingRemote wraps a backend with the hub's ReadReporter capability,
// standing in for the https:// backend in the multi-device harness.
type readReportingRemote struct {
	remote.Backend

	mu      sync.Mutex
	fail    bool
	reports [][]remote.ReadEvent
}

func (r *readReportingRemote) ReportReads(_ context.Context, reads []remote.ReadEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return fmt.Errorf("hub unreachable")
	}
	cp := make([]remote.ReadEvent, len(reads))
	copy(cp, reads)
	r.reports = append(r.reports, cp)
	return nil
}

func (r *readReportingRemote) setFail(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = v
}

func (r *readReportingRemote) all() [][]remote.ReadEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reports
}

// TestAgentReadReporting drives the read spool through real sync cycles: the
// queued reads flush to a reporting hub (deduped), survive an unreachable hub
// and retry, and never disturb the sync result itself.
func TestAgentReadReporting(t *testing.T) {
	hub := &readReportingRemote{Backend: sharedRemote(t)}
	a := newDevice(t, "deva", hub)
	write(t, a.Folder, "wiki/a.md", "content")

	// The agent read a.md twice and b.md once before this cycle.
	a.Store.LogRead("wiki/a.md")
	a.Store.LogRead("wiki/a.md")
	a.Store.LogRead("b.md")
	res := cycle(t, a)
	if !res.Pushed {
		t.Fatal("cycle should have pushed")
	}
	reports := hub.all()
	if len(reports) != 1 || len(reports[0]) != 2 {
		t.Fatalf("reports = %+v, want one deduped batch of 2", reports)
	}
	if reports[0][0].Path != "wiki/a.md" || reports[0][1].Path != "b.md" {
		t.Fatalf("batch = %+v", reports[0])
	}
	// Drained: an idle cycle reports nothing.
	cycle(t, a)
	if len(hub.all()) != 1 {
		t.Fatal("empty spool still produced a report")
	}

	// Hub down: the cycle still succeeds and the batch stays queued.
	hub.setFail(true)
	a.Store.LogRead("wiki/a.md")
	if res := cycle(t, a); res.Offline {
		t.Fatal("a failed read report must not mark the cycle offline")
	}
	if len(hub.all()) != 1 {
		t.Fatal("failed report should not have landed")
	}
	// Hub back: the next cycle retries the same batch.
	hub.setFail(false)
	cycle(t, a)
	reports = hub.all()
	if len(reports) != 2 || len(reports[1]) != 1 || reports[1][0].Path != "wiki/a.md" {
		t.Fatalf("retry reports = %+v", reports)
	}

	// A backend without the capability (plain object store) is untouched by
	// queued reads: the cycle runs, the spool just keeps waiting.
	b := newDevice(t, "devb", sharedRemote(t))
	write(t, b.Folder, "x.md", "x")
	b.Store.LogRead("x.md")
	cycle(t, b)
	if evs, err := b.Store.PendingReads(); err != nil || len(evs) != 1 {
		t.Fatalf("spool on a hubless device = %v, %v; want the read still queued", evs, err)
	}
}
