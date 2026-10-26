package webapp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bob is denied a/ by restricted(); liam is granted it.
func bobsView() pathFilter {
	return pathFilter{project: restricted(), email: "bob@x.io", base: PermWrite, on: true}
}

func TestFrameForFiltersPathsPerReader(t *testing.T) {
	enc := func(ev changeEvent) []byte {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	dec := func(b []byte) changeEvent {
		var ev changeEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}

	mixed := enc(changeEvent{Type: "change", Paths: []string{"a/x.md", "b.md"}, Puts: 2, Source: "sync"})
	out, ok := frameFor(mixed, bobsView())
	if ev := dec(out); !ok || len(ev.Paths) != 1 || ev.Paths[0] != "b.md" || ev.Puts != 0 || ev.Source != "sync" {
		t.Fatalf("mixed frame for bob = %s (sent %v), want only b.md and no counts", out, ok)
	}

	if out, ok := frameFor(enc(changeEvent{Type: "change", Paths: []string{"a/x.md"}, Puts: 1}), bobsView()); ok {
		t.Fatalf("a frame naming only a hidden path was sent: %s", out)
	}

	truncated := enc(changeEvent{Type: "change", Paths: []string{"a/x.md"}, More: true, Puts: 90})
	out, ok = frameFor(truncated, bobsView())
	if ev := dec(out); !ok || !ev.More || len(ev.Paths) != 0 || ev.Puts != 0 {
		t.Fatalf("truncated frame for bob = %s (sent %v), want more with no paths", out, ok)
	}

	roster := enc(changeEvent{Type: "presence", People: []person{{Name: "Alice", Path: "a/x.md"}, {Name: "Carol", Path: "b.md"}}})
	out, ok = frameFor(roster, bobsView())
	ev := dec(out)
	if !ok || len(ev.People) != 2 || ev.People[0].Path != "" || ev.People[1].Path != "b.md" {
		t.Fatalf("roster for bob = %s (sent %v), want Alice kept with no path", out, ok)
	}

	liam := pathFilter{project: restricted(), email: "liam@x.io", base: PermWrite, on: true}
	if out, ok := frameFor(mixed, liam); !ok || string(out) != string(mixed) {
		t.Fatalf("a reader granted a/ got %s, want the frame unchanged", out)
	}
	if out, ok := frameFor(mixed, pathFilter{}); !ok || string(out) != string(mixed) {
		t.Fatalf("an unfiltered reader got %s, want the frame unchanged", out)
	}
}

// streamAs opens the project's change stream as one member and hands back its
// frames. It returns once the hub holds the stream, so a write after it cannot
// race the subscription.
func streamAs(t *testing.T, srv *Server, ts *httptest.Server, project, email string, c *http.Cookie) <-chan changeEvent {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/p/"+project+"/events", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("events as %s: %d", email, resp.StatusCode)
	}
	frames := make(chan changeEvent, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev changeEvent
			if json.Unmarshal([]byte(line), &ev) == nil {
				frames <- ev
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !srv.events().hasActor(project, email) {
		if time.Now().After(deadline) {
			t.Fatalf("the hub never registered %s's stream", email)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return frames
}

func sendAs(t *testing.T, ts *httptest.Server, method, url, body string, c *http.Cookie) string {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+url, strings.NewReader(body))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, out)
	}
	return string(out)
}

// nextFrame returns the next frame of the given type, skipping others.
func nextFrame(t *testing.T, frames <-chan changeEvent, typ string) changeEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-frames:
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s frame within 5s", typ)
		}
	}
}

func TestEventStreamNeverNamesADeniedPath(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	ts := httptest.NewServer(h)
	// A cleanup, not a defer: Close waits for open connections, and the
	// stream's own cleanup (registered later, so run first) is what ends one.
	t.Cleanup(ts.Close)
	sendAs(t, ts, "PUT", "/api/p/"+p.ID+"/folders", `{"prefix":"secret","default":"none"}`, cookies["alice"])

	bob := streamAs(t, srv, ts, p.ID, "bob@x.io", cookies["bob"])
	sendAs(t, ts, "PUT", "/api/p/"+p.ID+"/upload/content?path=secret/plan.md", "hidden", cookies["alice"])
	sendAs(t, ts, "PUT", "/api/p/"+p.ID+"/upload/content?path=open/notes.md", "shared", cookies["alice"])

	// The hidden write's frame was dropped, so the first change bob sees is
	// the visible one.
	if ev := nextFrame(t, bob, "change"); len(ev.Paths) != 1 || ev.Paths[0] != "open/notes.md" {
		t.Fatalf("bob's first change frame = %+v, want only open/notes.md", ev)
	}

	sendAs(t, ts, "POST", "/api/p/"+p.ID+"/presence", `{"path":"secret/plan.md"}`, cookies["alice"])
	for _, who := range nextFrame(t, bob, "presence").People {
		if strings.HasPrefix(who.Path, "secret/") {
			t.Fatalf("bob's roster shows %s on %s", who.Name, who.Path)
		}
	}
	// The heartbeat's answer is the same roster, so it is filtered too.
	if body := sendAs(t, ts, "POST", "/api/p/"+p.ID+"/presence", `{"path":"open/notes.md"}`, cookies["bob"]); strings.Contains(body, "secret/") {
		t.Fatalf("bob's presence answer names a hidden path: %s", body)
	}
}

// A stream opened before a folder was restricted must not keep showing it:
// the rule change arrives on the stream itself, and every frame after it is
// filtered by the new rule.
func TestEventStreamRefiltersWhenAFolderIsRestricted(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	ts := httptest.NewServer(h)
	// A cleanup, not a defer: Close waits for open connections, and the
	// stream's own cleanup (registered later, so run first) is what ends one.
	t.Cleanup(ts.Close)

	bob := streamAs(t, srv, ts, p.ID, "bob@x.io", cookies["bob"])
	sendAs(t, ts, "PUT", "/api/p/"+p.ID+"/folders", `{"prefix":"secret","default":"none"}`, cookies["alice"])
	nextFrame(t, bob, "scope")
	sendAs(t, ts, "PUT", "/api/p/"+p.ID+"/upload/content?path=secret/plan.md", "hidden", cookies["alice"])
	sendAs(t, ts, "PUT", "/api/p/"+p.ID+"/upload/content?path=open/notes.md", "shared", cookies["alice"])

	if ev := nextFrame(t, bob, "change"); len(ev.Paths) != 1 || ev.Paths[0] != "open/notes.md" {
		t.Fatalf("bob's first change frame after the rule = %+v, want only open/notes.md", ev)
	}
}

// No frame crosses a rule change under the old filter, whatever order things
// reach the stream in. Here the rule lands in the registry and a write's frame
// is published with NO scope frame at all — standing in for a stream that
// subscribed just after the rule's scope frame went out, and for a change
// frame that was queued ahead of it.
func TestEventStreamFiltersByTheRulesAtDelivery(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	bob := streamAs(t, srv, ts, p.ID, "bob@x.io", cookies["bob"])
	if err := srv.Projects.SetFolder(p.ID, FolderRule{Prefix: "secret/", Default: PermNone}); err != nil {
		t.Fatal(err)
	}
	srv.events().publish(p.ID, changeEvent{Type: "change", Paths: []string{"secret/plan.md"}, Puts: 1})
	srv.events().publish(p.ID, changeEvent{Type: "change", Paths: []string{"open/notes.md"}, Puts: 1})
	if ev := nextFrame(t, bob, "change"); len(ev.Paths) != 1 || ev.Paths[0] != "open/notes.md" {
		t.Fatalf("bob's first change frame = %+v; a frame crossed the rule under the old filter", ev)
	}
}

// Frames and a narrowing rule race each other. A frame's rules are read after
// it was dequeued, so after the change it describes: a path reaches the reader
// only if the reader could read it at that instant. Two things follow, and
// this checks both under contention: a change made after the rule took effect
// never names the hidden path, and nothing names it once the scope frame is
// through, whatever order the fan-out queued the frames in.
func TestEventStreamHoldsTheRuleUnderConcurrentFrames(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	bob := streamAs(t, srv, ts, p.ID, "bob@x.io", cookies["bob"])
	for round := 0; round < 5; round++ {
		dir := fmt.Sprintf("secret%d/", round)
		var narrowed atomic.Bool
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 150; i++ {
					when := "early"
					if narrowed.Load() {
						when = "late"
					}
					path := fmt.Sprintf("%s%s-%d-%d.md", dir, when, w, i)
					if i%2 == 0 {
						srv.events().publish(p.ID, changeEvent{Type: "change", Paths: []string{path, "open/x.md"}, Puts: 2})
					} else {
						srv.events().publish(p.ID, changeEvent{Type: "presence", People: []person{{Name: "Carol", Path: path}}})
					}
					time.Sleep(100 * time.Microsecond)
				}
			}()
		}
		ruled := make(chan error, 1)
		go func() {
			time.Sleep(5 * time.Millisecond)
			req, _ := http.NewRequest("PUT", ts.URL+"/api/p/"+p.ID+"/folders", strings.NewReader(`{"prefix":"`+dir+`","default":"none"}`))
			req.AddCookie(cookies["alice"])
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode != 200 {
					err = fmt.Errorf("restricting %s: %d", dir, resp.StatusCode)
				}
			}
			narrowed.Store(true)
			ruled <- err
			wg.Wait()
			// Let the queue drain first: a frame dropped on overflow would be this one.
			time.Sleep(50 * time.Millisecond)
			srv.events().publish(p.ID, changeEvent{Type: "change", Paths: []string{"open/done.md"}, Puts: 1})
		}()

		scoped, resynced := false, false
		deadline := time.After(10 * time.Second)
		for done := false; !done; {
			select {
			case ev := <-bob:
				named := append([]string(nil), ev.Paths...)
				for _, who := range ev.People {
					named = append(named, who.Path)
				}
				for _, path := range named {
					if strings.HasPrefix(path, dir+"late-") {
						t.Fatalf("round %d: a change made after the rule reached bob: %+v", round, ev)
					}
					if scoped && strings.HasPrefix(path, dir) {
						t.Fatalf("round %d: a hidden path reached bob after the scope frame: %+v", round, ev)
					}
					done = done || path == "open/done.md"
				}
				scoped = scoped || ev.Type == "scope"
				resynced = resynced || ev.Type == "resync"
			case <-deadline:
				t.Fatalf("round %d: the stream never delivered the last frame", round)
			}
		}
		if err := <-ruled; err != nil {
			t.Fatal(err)
		}
		if !scoped && !resynced { // an overflow may drop the scope frame, and says so
			t.Fatalf("round %d: bob never saw the scope frame", round)
		}
	}
}
