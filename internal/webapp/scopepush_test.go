package webapp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/runbear-io/beardrive/internal/remote"
)

/* A folder rule change has to reach the devices it restricts.

   Nothing published an event when folder rules moved, so a device learned its
   new scope only when it next asked — which used to be every remote cycle and
   therefore never mattered. Once the daemon started standing down beside a
   healthy change stream (watchedPollFactor, docs/hub-load-prd.md Stage 1),
   "when it next asks" became up to five minutes.

   That is not a cosmetic delay. loadScope runs BEFORE the scan precisely so a
   scan cannot mint an op the hub will refuse, and a refused op is a 403 on the
   whole journal PUT — which wedges that device's sync until somebody edits its
   journal by hand. A narrowing rule that takes five minutes to arrive is five
   minutes in which an honest device can wedge itself.

   So the rule change goes out on the stream that is already open. The daemon's
   existing wake path does the rest: any frame clears the remote gate, and the
   next cycle re-reads scope before it scans. */

func TestFolderRuleChangeWakesADevice(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	be, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := be.(remote.Watcher).Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The stream has to be registered before the rule moves, or this measures
	// nothing: publish only reaches subscribers that already exist.
	waitForSubscriber(t, srv, p.ID)

	setFolderRule(t, ts, p.ID, `{"prefix":"private","default":"none"}`)

	select {
	case _, open := <-ch:
		if !open {
			t.Fatal("the change stream closed instead of reporting the rule change")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a folder rule changed and no frame reached a device holding an open " +
			"change stream; that device keeps its old scope until its next poll, which " +
			"is watchedPollFactor x remoteInterval away")
	}
}

// Clearing a rule widens or narrows a subtree just as setting one does, and
// the narrowing direction (a grant that came from the cleared rule going away)
// is the one with teeth.
func TestFolderRuleClearWakesADevice(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	setFolderRule(t, ts, p.ID, `{"prefix":"private","default":"none"}`)

	be, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := be.(remote.Watcher).Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitForSubscriber(t, srv, p.ID)

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/p/"+p.ID+"/folders?prefix=private", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clearing the rule: %s", resp.Status)
	}

	select {
	case _, open := <-ch:
		if !open {
			t.Fatal("the change stream closed instead of reporting the cleared rule")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a folder rule was cleared and no frame reached a device holding an " +
			"open change stream")
	}
}

func setFolderRule(t *testing.T, ts *httptest.Server, project, body string) {
	t.Helper()
	req, _ := http.NewRequest("PUT", ts.URL+"/api/p/"+project+"/folders", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setting the folder rule: %s", resp.Status)
	}
}

// waitForSubscriber blocks until the hub has registered the stream. Watch
// returns as soon as the response headers land, which can be before the
// handler has added itself to the fan-out map.
func waitForSubscriber(t *testing.T, s *Server, project string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h := s.events()
		h.mu.Lock()
		n := len(h.subs[project])
		h.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the hub never registered the change stream")
}
