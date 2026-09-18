package webapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// collabClient is one browser sitting in the editor: an SSE stream it reads
// continuously, plus updates it POSTs.
type collabClient struct {
	id      int
	// Written by this client's stream goroutine, read by the summary below
	// while those goroutines are still draining: close(stop) asks them to
	// finish, it does not wait for them. Every field they touch is therefore
	// read the way it is written.
	seed    atomic.Bool
	updates int32 // frames of type "update" received
	resyncs int32 // forced rebuilds — the symptom we are hunting
	badPost int32 // non-200 from a POST
	posted  int32
}

// awaitResync waits for the rebuild notice a dropped client is owed.
//
// The dropped frames themselves are gone — post() only ever queued 32 and
// discarded the rest — so the notice cannot ride them. It goes out just
// before the NEXT frame the stream writes, or on the keepalive tick if the
// room fell silent, which is up to 20s away. A busy CI box reaches that
// second case (the typists finish, nothing more is queued, and the notice
// waits for the tick) where a fast laptop never does.
func awaitResync(c *collabClient, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&c.resyncs) > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestCollabSevenEditorsOneFile drives seven real HTTP clients editing one file
// at once and reports what the relay did: who seeded, how many updates each
// peer actually received, and whether anyone was told to resync (a dropped
// frame, which costs that editor a full rebuild).
func TestCollabSevenEditorsOneFile(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// Cancel before Close (defers are LIFO): the SSE streams never end on
	// their own, and httptest.Close waits for every outstanding request.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		editors        = 7
		updatesPerEdit = 20
	)
	url := ts.URL + "/api/p/" + p.ID + "/collab?path=doc.md"

	clients := make([]*collabClient, editors)
	cid := func(i int) string { return fmt.Sprintf("client-%d", i) }
	var streamsUp sync.WaitGroup
	var readers sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < editors; i++ {
		c := &collabClient{id: i}
		clients[i] = c
		streamsUp.Add(1)
		readers.Add(1)
		go func() {
			defer readers.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", url+"&cid="+cid(c.id), nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("client %d: stream: %v", c.id, err)
				streamsUp.Done()
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("client %d: stream status %d", c.id, resp.StatusCode)
				streamsUp.Done()
				return
			}
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			gotHello := false
			for sc.Scan() {
				line := sc.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue // keepalive comment or blank separator
				}
				var f collabFrame
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f); err != nil {
					t.Errorf("client %d: bad frame: %v", c.id, err)
					continue
				}
				switch f.Type {
				case "hello":
					c.seed.Store(f.Seed)
					if !gotHello {
						gotHello = true
						streamsUp.Done()
					}
				case "update":
					atomic.AddInt32(&c.updates, 1)
				case "resync":
					atomic.AddInt32(&c.resyncs, 1)
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
		// Join in order so the seed claim is unambiguous, as a real room fills.
		streamsUp.Wait()
	}

	// Everyone types at once.
	var typing sync.WaitGroup
	for i := 0; i < editors; i++ {
		typing.Add(1)
		go func(c *collabClient) {
			defer typing.Done()
			for n := 0; n < updatesPerEdit; n++ {
				body, _ := json.Marshal(map[string]string{
					"cid": cid(c.id),
					"update": base64.StdEncoding.EncodeToString(
						[]byte(fmt.Sprintf("client-%d-update-%d", c.id, n))),
				})
				resp, err := http.Post(url, "application/json", bytes.NewReader(body))
				if err != nil {
					atomic.AddInt32(&c.badPost, 1)
					continue
				}
				resp.Body.Close()
				if resp.StatusCode != 200 {
					atomic.AddInt32(&c.badPost, 1)
					continue
				}
				atomic.AddInt32(&c.posted, 1)
				time.Sleep(3 * time.Millisecond) // keystroke cadence
			}
		}(clients[i])
	}
	typing.Wait()
	time.Sleep(500 * time.Millisecond) // let fan-out drain
	close(stop)

	// Every editor sees every update EXCEPT its own. The client id each
	// stream declares is what makes that possible over HTTP, where the POST
	// is a different request from the stream; without it the relay has no
	// sender to skip and mails everyone their own keystrokes back.
	want := (editors - 1) * updatesPerEdit
	seeders := 0
	for _, c := range clients {
		if c.seed.Load() {
			seeders++
		}
		t.Logf("client %d: seed=%v posted=%d received=%d (want %d) resyncs=%d badPost=%d",
			c.id, c.seed.Load(), atomic.LoadInt32(&c.posted),
			atomic.LoadInt32(&c.updates), want, atomic.LoadInt32(&c.resyncs),
			atomic.LoadInt32(&c.badPost))
	}
	if seeders != 1 {
		t.Errorf("seeders = %d, want exactly 1", seeders)
	}
	for _, c := range clients {
		if bad := atomic.LoadInt32(&c.badPost); bad != 0 {
			t.Errorf("client %d: %d POSTs failed", c.id, bad)
		}
		got := int(atomic.LoadInt32(&c.updates))
		switch {
		case got > want:
			t.Errorf("client %d: received %d updates, want %d — the relay is "+
				"echoing this client its own updates", c.id, got, want)
		case got < want && func() bool {
			awaitResync(c, 30*time.Second)
			return atomic.LoadInt32(&c.resyncs) == 0
		}():
			// The invariant that matters for a CRDT peer: frames may be
			// dropped when a client falls behind, but it must always be TOLD,
			// or it is silently diverged from everyone else.
			t.Errorf("client %d: received %d of %d updates and was never told "+
				"to resync — silent divergence", c.id, got, want)
		}
	}
	if logBuf.Len() > 0 {
		t.Logf("server log output:\n%s", logBuf.String())
	} else {
		t.Logf("server log: (silent)")
	}
}

// TestCollabSlowEditorAmongSeven is the mechanism that scales with the number
// of editors: fan-out is N-per-update, each subscriber queue holds subBuffer
// (32) frames, and a client that cannot drain that fast is marked lost and
// told to resync — which in collab.ts tears the EventSource down and opens a
// new one. Seven people typing at once is when a merely-busy browser starts
// missing that window.
func TestCollabSlowEditorAmongSeven(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		editors = 7
		burst   = 300
		slowIdx = 6 // one busy browser among seven
		// Big enough that the kernel socket buffer cannot hide the backlog:
		// with small frames everything fits in TCP and nothing is ever
		// dropped, it is just read late.
		payload = 4 << 10
	)
	url := ts.URL + "/api/p/" + p.ID + "/collab?path=doc.md"

	clients := make([]*collabClient, editors)
	cid := func(i int) string { return fmt.Sprintf("client-%d", i) }
	var up sync.WaitGroup
	for i := 0; i < editors; i++ {
		c := &collabClient{id: i}
		clients[i] = c
		up.Add(1)
		go func() {
			req, _ := http.NewRequestWithContext(ctx, "GET", url+"&cid="+cid(c.id), nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				up.Done()
				return
			}
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			hello := false
			for sc.Scan() {
				line := sc.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var f collabFrame
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f) != nil {
					continue
				}
				switch f.Type {
				case "hello":
					if !hello {
						hello = true
						up.Done()
					}
				case "update":
					atomic.AddInt32(&c.updates, 1)
				case "resync":
					atomic.AddInt32(&c.resyncs, 1)
				}
				if c.id == slowIdx {
					time.Sleep(20 * time.Millisecond) // parsing + applying Yjs
				}
			}
		}()
		up.Wait()
	}

	var typing sync.WaitGroup
	for i := 0; i < editors; i++ {
		if i == slowIdx {
			continue // the slow one is reading, not typing
		}
		typing.Add(1)
		go func(c *collabClient) {
			defer typing.Done()
			for n := 0; n < burst; n++ {
				body, _ := json.Marshal(map[string]string{
					"cid": cid(c.id),
					"update": base64.StdEncoding.EncodeToString(
						[]byte(fmt.Sprintf("c%d-n%d-%s", c.id, n, strings.Repeat("x", payload)))),
				})
				resp, err := http.Post(url, "application/json", bytes.NewReader(body))
				if err != nil {
					atomic.AddInt32(&c.badPost, 1)
					continue
				}
				resp.Body.Close()
				if resp.StatusCode != 200 {
					atomic.AddInt32(&c.badPost, 1)
					continue
				}
				atomic.AddInt32(&c.posted, 1)
			}
		}(clients[i])
	}
	typing.Wait()
	// Drain until quiet: a slow reader is still working through TCP buffers
	// long after the last POST, and calling that a drop would be wrong.
	// The slow client types nothing, so it should receive every typist's
	// burst; a typist receives everyone's but its own.
	sent := (editors - 1) * burst
	typistWant := (editors - 2) * burst
	prev, quiet := int32(-1), 0
	for i := 0; i < 600 && quiet < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		now := atomic.LoadInt32(&clients[slowIdx].updates)
		if now == prev {
			quiet++
		} else {
			quiet = 0
		}
		prev = now
		if int(now) >= sent {
			break
		}
	}

	for _, c := range clients {
		want := typistWant
		if c.id == slowIdx {
			want = sent
		}
		t.Logf("client %d%s: posted=%d received=%d/%d resyncs=%d badPost=%d",
			c.id, map[bool]string{true: " (SLOW)"}[c.id == slowIdx],
			atomic.LoadInt32(&c.posted), atomic.LoadInt32(&c.updates), want,
			atomic.LoadInt32(&c.resyncs), atomic.LoadInt32(&c.badPost))
		// A busy machine can back any client up past the 32-frame queue, so
		// "received everything" is not a safe assertion. What must hold is
		// that a client which missed frames was told to rebuild.
		got := int(atomic.LoadInt32(&c.updates))
		if got > want {
			t.Errorf("client %d: received %d, want at most %d — self-echo",
				c.id, got, want)
		}
		if got < want {
			awaitResync(c, 30*time.Second)
			if atomic.LoadInt32(&c.resyncs) == 0 {
				t.Errorf("client %d: received %d of %d and was never told to "+
					"resync — silent divergence", c.id, got, want)
			}
		}
	}
	slow := clients[slowIdx]
	if r := atomic.LoadInt32(&slow.resyncs); r > 0 {
		t.Logf("CONFIRMED: the slow editor was told to resync %d times "+
			"(each one tears down its EventSource and re-subscribes)", r)
	}
	if logBuf.Len() > 0 {
		t.Logf("server log output:\n%s", logBuf.String())
	} else {
		t.Logf("server log: (silent)")
	}
}

// TestCollabRoomFullWithSevenEditors exercises the one limit seven people can
// genuinely reach by typing: maxRoomBytes (8 MiB of update log). N editors
// fill it N times faster, and the answer is a reset plus "everyone rebuild" —
// so the thing to check is that the reset is safe while four other people are
// still posting into it.
func TestCollabRoomFullWithSevenEditors(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	url := ts.URL + "/api/p/" + p.ID + "/collab?path=doc.md"

	chunk := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("y"), 900<<10))
	post := func() (full bool, code int) {
		body, _ := json.Marshal(map[string]string{"update": chunk})
		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			return false, 0
		}
		defer resp.Body.Close()
		var out struct {
			OK   bool `json:"ok"`
			Full bool `json:"full"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		return out.Full, resp.StatusCode
	}

	// Five editors hammering one room, concurrently, past the cap.
	var wg sync.WaitGroup
	var fulls, bad int32
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 6; n++ {
				full, code := post()
				if code != 200 {
					atomic.AddInt32(&bad, 1)
					continue
				}
				if full {
					atomic.AddInt32(&fulls, 1)
				}
			}
		}()
	}
	wg.Wait()

	t.Logf("42 posts x 900KB across 7 editors: full=%d badStatus=%d", fulls, bad)
	if bad != 0 {
		t.Errorf("%d posts returned a non-200 — seven editors should never be refused", bad)
	}
	if fulls == 0 {
		t.Errorf("room never reported full; cap not reached, test is not exercising it")
	}
	// After the reset the room must still work: a fresh post is accepted.
	if full, code := post(); code != 200 {
		t.Errorf("post after reset: status %d", code)
	} else {
		t.Logf("post after reset: ok (full=%v)", full)
	}
	if logBuf.Len() > 0 {
		t.Logf("server log output:\n%s", logBuf.String())
	} else {
		t.Logf("server log: (silent)")
	}
}
