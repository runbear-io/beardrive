package remote

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Round 11 — the slow-loris round 10 carried as "I did not want a 5-minute
// test".
//
// The threat is the hub, not the client: a folder's remote URL comes out of
// .bdrive/config.json, which travels with the folder, and a hub that never
// finishes a response holds this device's sync cycle — which runs under the
// volume flock, so a hung cycle blocks the daemon AND every `bdrive sync`,
// `bdrive forget` and `bdrive log` on that mount, forever.
//
// The cheap form of the test is the right one: pin that a whole-request
// deadline is CONFIGURED on every client the device uses (round 10's finding
// that initClient had no CheckRedirect came from nobody reading the client's
// construction), and prove the mechanism separately against a dribbling
// server with the deadline turned down.
//
// Helpers are prefixed sec11.

// sec11Backend opens the https:// sync backend against a test server, the way
// remote.Open does for a hub URL out of a folder's config.
func sec11Backend(t *testing.T, base string) *httpBackend {
	t.Helper()
	b, err := newHTTPBackend(base + "/p/test")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSec_Slow_TheSyncBackendCarriesAWholeRequestDeadline
//
// http.Client's zero value has NO timeout: a response that never ends is a
// goroutine and a file descriptor held until the process dies. Only
// Client.Timeout covers the whole exchange including the body — a Transport's
// ResponseHeaderTimeout would let a hub send headers instantly and then dribble
// a blob forever, which is the exact shape of this attack.
//
// CheckRedirect is pinned in the same breath because it is the other field
// whose absence is invisible: without it the client follows a hub's 3xx to any
// host, carrying this device's bearer token.
func TestSec_Slow_TheSyncBackendCarriesAWholeRequestDeadline(t *testing.T) {
	b := sec11Backend(t, "https://hub.example")
	if b.hc == nil {
		t.Fatal("the sync backend has no http.Client of its own")
	}
	if b.hc.Timeout <= 0 {
		t.Fatalf("the sync backend's client has Timeout=%v — a hub that never finishes a "+
			"response holds the cycle, and the cycle holds the volume flock", b.hc.Timeout)
	}
	if b.hc.Timeout != 5*time.Minute {
		t.Errorf("the sync backend's client Timeout is %v, was 5m — this is the only bound on "+
			"how long a hub can hold a sync cycle open", b.hc.Timeout)
	}
	if b.hc.CheckRedirect == nil {
		t.Error("the sync backend's client has no CheckRedirect — a hub's 3xx would carry this " +
			"device's bearer token to any host net/http considers the same hostname")
	}
	if b.hc == http.DefaultClient {
		t.Error("the sync backend uses http.DefaultClient, which has no timeout at all")
	}
}

// TestSec_Slow_ADribblingHubCannotHoldASyncCycleOpenPastTheDeadline
//
// The mechanism, proven rather than assumed, with the real value turned down
// so the test costs a second instead of five minutes.
//
// The server answers 200 with headers immediately and then writes one byte
// every 50ms and never ends the body — a textbook slow loris, and the case a
// header-only or dial-only timeout does not cover. Get() therefore SUCCEEDS
// (headers arrived); the deadline has to bite on the body read the syncer does
// afterwards, which is the part that would otherwise never return.
func TestSec_Slow_ADribblingHubCannotHoldASyncCycleOpenPastTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576") // a blob we will never finish
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		for {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done(): // the client gave up — let the handler exit
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	b := sec11Backend(t, srv.URL)
	b.hc.Timeout = time.Second // the real value is 5m; the mechanism is the same field

	done := make(chan error, 1)
	go func() {
		rc, err := b.Get(context.Background(), "blobs/deadbeef")
		if err != nil {
			done <- err
			return
		}
		defer rc.Close()
		_, err = io.Copy(io.Discard, rc)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the dribbling body was read to completion, which cannot happen")
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("a hub that sends one byte every 50ms and never ends the body held the read "+
			"for 15s against a %v client deadline — nothing bounds the whole exchange, so a "+
			"sync cycle (and the volume flock it holds) hangs on the hub's whim", b.hc.Timeout)
	}
}

// TestSec_Slow_AHubThatAcceptsHeadersAndNeverAnswersIsAlsoBounded
//
// The other half of the same window: headers that never arrive at all. This is
// what a Transport-level ResponseHeaderTimeout would cover and Client.Timeout
// must cover too — there is no ResponseHeaderTimeout configured on this
// backend, so Timeout is the only thing standing here.
func TestSec_Slow_AHubThatAcceptsHeadersAndNeverAnswersIsAlsoBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never write a status
	}))
	defer srv.Close()

	b := sec11Backend(t, srv.URL)
	b.hc.Timeout = time.Second

	done := make(chan error, 1)
	go func() {
		_, err := b.Exists(context.Background(), "blobs/deadbeef")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Exists returned success from a server that never answered")
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("a hub that accepts the request and never sends a status header held the "+
			"cycle for 15s against a %v client deadline", b.hc.Timeout)
	}
}
