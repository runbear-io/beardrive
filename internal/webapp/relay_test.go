package webapp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

/* A write on one hub process has to reach clients on another.

   eventHub.publish fans a frame out to the subscribers THIS process holds.
   With one process that is everything; behind two it means a teammate's write
   on instance A reaches nobody connected to instance B — their tree never
   updates, an open file never refreshes, and a device's change stream sits
   silent until its own poll notices. Cloud Run gives processes no way to talk
   to each other, so it has to be carried (docs/hub-load-prd.md Stage 9). */

// The contrast, kept permanently: this is what the hub does TODAY, and what
// every single-instance deployment should keep doing. It is also what makes
// the test below mean something.
func TestWithoutARelayAFrameStaysInItsProcess(t *testing.T) {
	a, b := &Server{}, &Server{}
	subB := mustSubscribe(t, b, "p1")
	defer b.events().unsubscribe("p1", subB)

	a.events().publish("p1", changeEvent{Type: "change", Paths: []string{"notes.md"}})

	if frame, ok := waitFrame(subB, 200*time.Millisecond); ok {
		t.Fatalf("a hub with no relay reached another process: %s", frame)
	}
}

func TestARelayCarriesAFrameToAnotherProcess(t *testing.T) {
	bus := newMemRelay()
	a, b := &Server{}, &Server{}
	a.UseRelay(bus.join())
	b.UseRelay(bus.join())

	subB := mustSubscribe(t, b, "p1")
	defer b.events().unsubscribe("p1", subB)

	a.events().publish("p1", changeEvent{Type: "change", Paths: []string{"notes.md"}, Puts: 1})

	frame, ok := waitFrame(subB, 3*time.Second)
	if !ok {
		t.Fatal("a write on one process never reached a client on another; " +
			"behind two instances that client's tree simply stops updating")
	}
	var ev changeEvent
	if err := json.Unmarshal(frame, &ev); err != nil {
		t.Fatalf("relayed frame is not a change frame: %v (%s)", err, frame)
	}
	if ev.Type != "change" || len(ev.Paths) != 1 || ev.Paths[0] != "notes.md" {
		t.Fatalf("relayed frame lost its content: %+v", ev)
	}
}

// The other half, and the one that is easy to get wrong: both transports
// broadcast to every listener INCLUDING the publisher, so without an origin
// check a client would be told about every change twice and refetch twice.
func TestARelayDoesNotEchoToThePublisher(t *testing.T) {
	bus := newMemRelay()
	a := &Server{}
	a.UseRelay(bus.join())
	// A second member, so the bus really is broadcasting rather than
	// short-circuiting a lone participant.
	b := &Server{}
	b.UseRelay(bus.join())

	subA := mustSubscribe(t, a, "p1")
	defer a.events().unsubscribe("p1", subA)

	a.events().publish("p1", changeEvent{Type: "change", Paths: []string{"notes.md"}})

	// The local fan-out delivers exactly one.
	if _, ok := waitFrame(subA, 3*time.Second); !ok {
		t.Fatal("the publisher's own subscriber never got the frame")
	}
	if frame, ok := waitFrame(subA, 300*time.Millisecond); ok {
		t.Fatalf("the publisher received its own frame back through the relay: %s", frame)
	}
}

// A frame too big for the transport must degrade to "resync", never to a
// truncated path list — a client that is handed half the paths believes it
// has been told about all of them.
func TestAnOversizedFrameBecomesAResync(t *testing.T) {
	bus := newMemRelay()
	a, b := &Server{}, &Server{}
	a.UseRelay(bus.join())
	b.UseRelay(bus.join())

	subB := mustSubscribe(t, b, "p1")
	defer b.events().unsubscribe("p1", subB)

	huge := make([]string, 64)
	for i := range huge {
		huge[i] = strings.Repeat("deeply/nested/path/segment/", 12) + "file.md"
	}
	a.events().publish("p1", changeEvent{Type: "change", Paths: huge, Puts: len(huge)})

	frame, ok := waitFrame(subB, 3*time.Second)
	if !ok {
		t.Fatal("an oversized frame was dropped instead of degraded; the far side " +
			"is now silently out of date")
	}
	var ev changeEvent
	if err := json.Unmarshal(frame, &ev); err != nil {
		t.Fatalf("not a frame: %v", err)
	}
	if ev.Type != "resync" {
		t.Fatalf("an oversized frame crossed as %q with %d paths; it must become a "+
			"resync rather than a partial truth", ev.Type, len(ev.Paths))
	}
}

// --- helpers ---------------------------------------------------------------

func mustSubscribe(t *testing.T, s *Server, project string) *subscriber {
	t.Helper()
	sub, ok := s.events().subscribe(project, "")
	if !ok {
		t.Fatal("subscribe refused")
	}
	return sub
}

func waitFrame(sub *subscriber, d time.Duration) ([]byte, bool) {
	select {
	case f := <-sub.ch:
		return f, true
	case <-time.After(d):
		return nil, false
	}
}
