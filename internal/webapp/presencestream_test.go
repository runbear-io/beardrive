package webapp

import (
	"testing"
	"time"
)

/* Presence used to be a 10-second heartbeat.

   Every open tab POSTed /presence every 10s, per TAB, forever — unlike the
   change stream, which is leader-elected and therefore one per browser. In
   production that was ~19% of every request the hub served, sent by people who
   were reading a page and doing nothing at all. One idle tab spent 8,640
   requests a day announcing that it still existed (docs/hub-load-prd.md).

   The information was already on the wire. A member holding an open change
   stream is, by definition, here: the hub has their connection in its hands.
   So liveness becomes a property of that connection and the beat goes away —
   what is left is an uplink on navigation, which is an event, not a timer.

   The TTL stays as a backstop for a client that reports presence WITHOUT
   holding a stream (an older bundle, a browser with no EventSource). These
   tests pin the two halves that matter: a streaming client does not expire,
   and losing the stream does remove them. */

func TestPresenceOutlivesTheTTLWhileTheStreamIsOpen(t *testing.T) {
	s := &Server{}
	const project, actor = "p1", "reader@example.com"

	sub, ok := s.events().subscribe(project, actor)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer s.events().unsubscribe(project, sub)

	t0 := time.Now()
	if _, _, ok := s.markPresence(project, actor, "Reader", "notes.md", t0); !ok {
		t.Fatal("the first mark was refused")
	}

	// Somebody else arrives long after the TTL would have expired a silent
	// reader. Expiry is lazy, computed on the next mark — so this is the
	// moment the old behaviour dropped them.
	people, _, _ := s.markPresence(project, "other@example.com", "Other", "", t0.Add(10*presenceTTL))

	if !hasPerson(people, "Reader") {
		t.Fatalf("a member holding an open change stream was expired from the roster "+
			"after %v of not beating; with the heartbeat gone that is everyone, "+
			"immediately. roster=%v", 10*presenceTTL, people)
	}
}

func TestPresenceEndsWhenTheStreamDoes(t *testing.T) {
	s := &Server{}
	const project, actor = "p1", "reader@example.com"

	sub, ok := s.events().subscribe(project, actor)
	if !ok {
		t.Fatal("subscribe refused")
	}
	// A second member, so there is somebody left to hold the room open and
	// receive the departure.
	other, ok := s.events().subscribe(project, "other@example.com")
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer s.events().unsubscribe(project, other)

	t0 := time.Now()
	s.markPresence(project, actor, "Reader", "notes.md", t0)
	s.markPresence(project, "other@example.com", "Other", "", t0)

	// The reader closes the tab. Nothing polls, so if this does not remove
	// them, nothing ever will and they haunt the roster until a restart.
	s.events().unsubscribe(project, sub)
	s.streamGone(project, actor)

	people, _, _ := s.markPresence(project, "other@example.com", "Other", "", t0)
	if hasPerson(people, "Reader") {
		t.Fatalf("a member whose change stream closed is still on the roster; "+
			"with no heartbeat there is no TTL to catch them. roster=%v", people)
	}
}

// A client that reports presence but holds no stream must still expire — that
// is the whole reason the TTL survives this change.
func TestPresenceStillExpiresWithoutAStream(t *testing.T) {
	s := &Server{}
	const project = "p1"

	t0 := time.Now()
	s.markPresence(project, "ghost@example.com", "Ghost", "", t0)
	people, _, _ := s.markPresence(project, "other@example.com", "Other", "", t0.Add(2*presenceTTL))

	if hasPerson(people, "Ghost") {
		t.Fatalf("a member with no open stream never expired, so the TTL backstop "+
			"is gone and a client that cannot stream is immortal. roster=%v", people)
	}
}

func hasPerson(people []person, name string) bool {
	for _, p := range people {
		if p.Name == name {
			return true
		}
	}
	return false
}
