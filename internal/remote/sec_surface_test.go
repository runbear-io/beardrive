package remote

// Round 7, scoreboard row 19 — the device as client of a hostile hub. The
// CISO's note on this row after round 6: "it still covers what the hub SAYS
// and not what it SERVES: a 10 GB body for a 3-byte blob, a Content-Type the
// viewer trusts, a journal 200 that is an HTML error page."
//
// Helpers are prefixed sec7.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSec_HTTP_AHubCannotMakeADeviceAllocateWithoutBound
//
// httpBackend.List is the first call of every sync cycle, on every device, on
// a ten-second timer, and it decodes the hub's answer with no limit at all:
//
//	var out struct{ Objects []Object `json:"objects"` }
//	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {  // http.go:211
//
// Every other body this package or its CLI reads is bounded — httpError caps
// at 512 bytes (http.go:186), the hub's own CLIAuth handlers cap at 64 KiB —
// but the one response that is a LIST, i.e. the one whose size the hub alone
// chooses, is read whole into memory. A hub (compromised, or simply the wrong
// one: `bdrive login <url>` exists to point a device at a hub it has never
// seen, on a URL somebody handed the user) answers one ordinary /store/list
// with as much JSON as it likes and every device in the org allocates it,
// again on the next tick, and the next.
//
// Round 4 already established that this listing is untrusted input — the keys
// in it become local journal file names and tar member names, which is why
// List filters them (http.go:220). The filter runs after the whole thing is in
// memory.
//
// The secure behavior asserted: a device bounds what it accepts from a hub, so
// an over-large listing is an error rather than an allocation.
func TestSec_HTTP_AHubCannotMakeADeviceAllocateWithoutBound(t *testing.T) {
	// Control: an ordinary listing is accepted, so the assertion below is
	// about the size and not about the harness.
	t.Run("control_small_listing", func(t *testing.T) {
		hub := sec7ListHub(t, 3)
		be := sec7Backend(t, hub)
		objs, err := be.List(context.Background(), "journal/")
		if err != nil {
			t.Fatalf("control: ordinary listing refused: %v", err)
		}
		if len(objs) != 3 {
			t.Fatalf("control: got %d objects, want 3", len(objs))
		}
	})

	// ~64 MiB of well-formed JSON from one /store/list answer.
	const objects = 700_000
	hub := sec7ListHub(t, objects)
	be := sec7Backend(t, hub)
	objs, err := be.List(context.Background(), "journal/")
	if err != nil {
		return // bounded: the secure outcome
	}
	t.Errorf("the hub answered one /store/list with %d objects (~%d MiB of JSON) and the "+
		"device accepted all %d of them: json.NewDecoder(resp.Body) with no io.LimitReader, "+
		"on the call every sync cycle starts with",
		objects, objects*96>>20, len(objs))
}

// sec7ListHub is a hub that answers /store/list with n objects and nothing
// else. The keys are well-formed so the finding is about the SIZE alone.
func sec7ListHub(t *testing.T, n int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"objects":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"key":"journal/dev%08d.jsonl","size":1}`, i)
		}
		fmt.Fprint(w, `]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sec7Backend(t *testing.T, hub *httptest.Server) Backend {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	be, err := Open(ctx, hub.URL+"/p/proj1234")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	return be
}
