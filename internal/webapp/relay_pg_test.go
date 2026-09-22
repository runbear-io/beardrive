package webapp

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

/* The relay against a real Postgres.

   The memory relay in relay_test.go pins the SHAPE — a frame crosses, an echo
   does not, an oversized frame degrades. None of that exercises the thing that
   can actually be wrong in production: whether LISTEN/NOTIFY is wired
   correctly, whether the payload survives a round trip through a database, and
   whether the ceiling is where this code thinks it is.

   Runs only when BDRIVE_TEST_POSTGRES is set, which the meta-postgres CI job
   now does. Before that job existed this file would have run for nobody, which
   is exactly the trap db_conformance_test.go was already in. */

func pgRelayDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BDRIVE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("BDRIVE_TEST_POSTGRES not set")
	}
	return dsn
}

func openPGRelay(t *testing.T, dsn string) (*Server, *pgRelay) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r, err := NewPostgresRelay(db, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	s := &Server{}
	s.UseRelay(r)
	return s, r
}

func TestPostgresRelayCarriesAFrameBetweenProcesses(t *testing.T) {
	dsn := pgRelayDSN(t)
	a, _ := openPGRelay(t, dsn)
	b, _ := openPGRelay(t, dsn)

	sub := mustSubscribe(t, b, "p1")
	defer b.events().unsubscribe("p1", sub)

	// LISTEN is established inside start(), but the notification only reaches
	// a listener that is already waiting — publish until one lands rather than
	// racing the first one.
	deadline := time.After(15 * time.Second)
	for {
		a.events().publish("p1", changeEvent{Type: "change", Paths: []string{"notes.md"}, Puts: 1})
		select {
		case frame := <-sub.ch:
			var ev changeEvent
			if err := json.Unmarshal(frame, &ev); err != nil {
				t.Fatalf("relayed frame is not a change frame: %v (%s)", err, frame)
			}
			if ev.Type != "change" || len(ev.Paths) != 1 || ev.Paths[0] != "notes.md" {
				t.Fatalf("relayed frame lost its content crossing postgres: %+v", ev)
			}
			return
		case <-deadline:
			t.Fatal("a write on one hub process never reached a client on another " +
				"over LISTEN/NOTIFY; behind two instances that client stops updating")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Postgres delivers a NOTIFY to every LISTENer including the one that sent it,
// so this is the case the origin check exists for. Without it every client
// would be told about each change twice and refetch twice.
func TestPostgresRelayDoesNotEchoToThePublisher(t *testing.T) {
	dsn := pgRelayDSN(t)
	a, _ := openPGRelay(t, dsn)
	b, _ := openPGRelay(t, dsn)

	subA := mustSubscribe(t, a, "p1")
	defer a.events().unsubscribe("p1", subA)
	subB := mustSubscribe(t, b, "p1")
	defer b.events().unsubscribe("p1", subB)

	// Wait until the far side is demonstrably receiving, so "A got one frame"
	// is a real result and not just a listener that had not connected yet.
	deadline := time.After(15 * time.Second)
	for ready := false; !ready; {
		a.events().publish("p1", changeEvent{Type: "change", Paths: []string{"warm.md"}})
		select {
		case <-subB.ch:
			ready = true
		case <-deadline:
			t.Fatal("the far side never received anything; nothing about echo was tested")
		case <-time.After(250 * time.Millisecond):
		}
	}
	// Drain whatever the warm-up put in A's own queue (its local fan-out).
	for draining := true; draining; {
		select {
		case <-subA.ch:
		case <-time.After(500 * time.Millisecond):
			draining = false
		}
	}

	a.events().publish("p1", changeEvent{Type: "change", Paths: []string{"once.md"}})

	// Exactly one: the local fan-out. Anything after it came back off the wire.
	if _, ok := waitFrame(subA, 5*time.Second); !ok {
		t.Fatal("the publisher's own subscriber never got the frame")
	}
	if frame, ok := waitFrame(subA, 2*time.Second); ok {
		t.Fatalf("the publisher received its own frame back through postgres: %s", frame)
	}
}

// The ceiling is Postgres's, not ours: NOTIFY refuses a payload over 8000
// bytes outright. A frame that big has to degrade to a resync, or the far side
// silently never hears about the write at all.
func TestPostgresRelayDegradesAnOversizedFrame(t *testing.T) {
	dsn := pgRelayDSN(t)
	a, _ := openPGRelay(t, dsn)
	b, _ := openPGRelay(t, dsn)

	sub := mustSubscribe(t, b, "p1")
	defer b.events().unsubscribe("p1", sub)

	huge := make([]string, 64)
	for i := range huge {
		huge[i] = strings.Repeat("deeply/nested/path/segment/", 12) + "file.md"
	}
	deadline := time.After(15 * time.Second)
	for {
		a.events().publish("p1", changeEvent{Type: "change", Paths: huge, Puts: len(huge)})
		select {
		case frame := <-sub.ch:
			var ev changeEvent
			if err := json.Unmarshal(frame, &ev); err != nil {
				t.Fatalf("not a frame: %v", err)
			}
			if ev.Type != "resync" {
				t.Fatalf("an oversized frame crossed as %q with %d paths; a partial "+
					"path list is worse than a resync because the client believes it", ev.Type, len(ev.Paths))
			}
			return
		case <-deadline:
			t.Fatal("an oversized frame was dropped rather than degraded, so the far " +
				"side is silently out of date")
		case <-time.After(250 * time.Millisecond):
		}
	}
}
