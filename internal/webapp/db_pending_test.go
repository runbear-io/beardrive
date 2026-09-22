package webapp

import (
	"testing"
	"time"
)

/* Short-lived sign-in state, in a place a second process can see.

   `bdrive login` spans three requests — mint, approve in a browser, poll —
   and a browser sign-in spans two, because an OAuth provider redirects back
   to a callback. All of that state lives in maps inside one process today
   (authcli.go's `pending`, mcpauth.go's `codes`, the cloud provider's OAuth
   state nonce), so every one of those flows requires the same instance to
   answer every hop. On two instances behind an ordinary load balancer that is
   a coin flip per hop, which is why raising max_instance_count above 1 is
   blocked on this and not on the journal (docs/hub-load-prd.md Phase 3).

   It also costs something today: a hub restart mid-login cancels the login,
   because the map went with the process.

   The payload is OPAQUE to the store. A pending CLI grant, an OAuth nonce and
   a consent code have nothing in common except a key, a deadline and single
   use — encoding what they hold here would put four owners' shapes in one
   schema for no gain. */

func TestPendingRepoConformance(t *testing.T) {
	for _, b := range metaBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.reset(t)
			s := b.open(t)
			defer s.Close()
			r := s.Pending()

			exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			g := PendingGrant{Kind: "cli", Key: "k1", Payload: []byte(`{"user":"a@x.io"}`), Expires: exp}
			if err := r.Put(g); err != nil {
				t.Fatal(err)
			}

			// Get does NOT consume: the device-flow poll reads the same grant
			// every second until a human approves it.
			for i := 0; i < 3; i++ {
				got, ok, err := r.Get("cli", "k1")
				if err != nil || !ok {
					t.Fatalf("Get #%d = (%v, %v)", i, ok, err)
				}
				if string(got.Payload) != string(g.Payload) {
					t.Fatalf("payload round-trip: got %q want %q", got.Payload, g.Payload)
				}
				if !got.Expires.Equal(exp) {
					t.Fatalf("expiry round-trip: got %v want %v", got.Expires, exp)
				}
			}

			// Put again replaces: a device grant is approved by rewriting it.
			g.Payload = []byte(`{"user":"a@x.io","granted":true}`)
			if err := r.Put(g); err != nil {
				t.Fatal(err)
			}
			got, _, _ := r.Get("cli", "k1")
			if string(got.Payload) != string(g.Payload) {
				t.Fatalf("Put did not replace: %q", got.Payload)
			}

			// The kind is part of the identity, not a label: two owners may
			// use the same key space without colliding.
			if _, ok, _ := r.Get("oauth-state", "k1"); ok {
				t.Fatal("a grant leaked across kinds")
			}

			// Take consumes. This is THE property: two pollers must not both
			// win one code, so the read and the delete are one step.
			took, ok, err := r.Take("cli", "k1")
			if err != nil || !ok {
				t.Fatalf("Take = (%v, %v)", ok, err)
			}
			if string(took.Payload) != string(g.Payload) {
				t.Fatalf("Take returned %q", took.Payload)
			}
			if _, ok, _ := r.Take("cli", "k1"); ok {
				t.Fatal("a single-use grant was taken twice")
			}
			if _, ok, _ := r.Get("cli", "k1"); ok {
				t.Fatal("a taken grant is still readable")
			}

			// Delete is the explicit cancel (a CLI that gave up).
			if err := r.Put(PendingGrant{Kind: "cli", Key: "k2", Expires: exp}); err != nil {
				t.Fatal(err)
			}
			if err := r.Delete("cli", "k2"); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := r.Get("cli", "k2"); ok {
				t.Fatal("Delete left the grant behind")
			}

			// An expired grant is gone whether or not anything pruned it.
			// Expiry that depends on a sweeper having run is expiry that does
			// not hold, and this one gates sign-in.
			past := time.Now().Add(-time.Minute)
			if err := r.Put(PendingGrant{Kind: "cli", Key: "old", Expires: past}); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := r.Get("cli", "old"); ok {
				t.Fatal("an expired grant was readable")
			}
			if _, ok, _ := r.Take("cli", "old"); ok {
				t.Fatal("an expired grant was takeable")
			}
			// Prune reclaims space, and must reclaim only the dead: a sweeper
			// that takes a live sign-in with it cancels somebody's login.
			if err := r.Put(PendingGrant{Kind: "cli", Key: "live", Payload: []byte("x"), Expires: exp}); err != nil {
				t.Fatal(err)
			}
			if err := r.Put(PendingGrant{Kind: "cli", Key: "dead", Payload: []byte("x"), Expires: past}); err != nil {
				t.Fatal(err)
			}
			if err := r.Prune(time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := r.Get("cli", "live"); !ok {
				t.Fatal("Prune took a grant that had not expired")
			}
			if _, ok, _ := r.Take("cli", "dead"); ok {
				t.Fatal("Prune left an expired grant behind")
			}

			// And it is all still there for the NEXT process, which is the
			// entire point.
			if err := r.Put(PendingGrant{Kind: "cli", Key: "k3", Payload: []byte("keep"), Expires: exp}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s2 := b.open(t)
			defer s2.Close()
			again, ok, err := s2.Pending().Get("cli", "k3")
			if err != nil || !ok {
				t.Fatalf("after reopen: Get = (%v, %v) — a pending sign-in did not survive the process", ok, err)
			}
			if string(again.Payload) != "keep" {
				t.Fatalf("after reopen: payload %q", again.Payload)
			}
		})
	}
}
