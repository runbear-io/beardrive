package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

/* Concurrent writers to one path, and the invariant that matters: no write
   that the hub ACCEPTED may have been built on a version that was already
   gone.

   The check is a chain. Every 200 reports the sha it produced, and every
   request declares the sha it was based on, so the accepted writes must link
   end to end: base(n) == sha(n-1), all the way back to the file's first
   version. A single break in that chain is a lost update — somebody's text
   overwritten by a writer who never saw it, which is the failure two browsers
   inflicted on each other every few seconds before If-Match existed.

   Run with -race: the whole point is a check-then-write under contention. */
func TestStress_ConcurrentWritersNeverLoseAnUpdate(t *testing.T) {
	const (
		writers = 8
		rounds  = 10
	)
	h := writableDirServer(t, map[string]string{"hot.md": "v0\n"})
	const url = "/api/upload/content?path=hot.md"

	// The starting version, so the chain has a root.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/file?path=hot.md", nil))
	head := strings.Trim(rec.Header().Get("ETag"), `"`)
	if head == "" {
		t.Fatal("no ETag on the file: nothing to base a write on")
	}

	type write struct{ base, sha string }
	var (
		mu       sync.Mutex
		accepted []write
		конфликт int // 409s, which are the healthy outcome under contention
	)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			mu.Lock()
			base := head // everyone starts from the same version, as two tabs do
			mu.Unlock()
			for i := 0; i < rounds; i++ {
				body := fmt.Sprintf("writer %d round %d\n", w, i)
				rec := httptest.NewRecorder()
				r := httptest.NewRequest("PUT", url, strings.NewReader(body))
				r.Header.Set("If-Match", base)
				h.ServeHTTP(rec, r)
				switch rec.Code {
				case http.StatusOK:
					var out struct{ SHA string }
					if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.SHA == "" {
						t.Errorf("accepted write reported no sha: %s", rec.Body.String())
						return
					}
					mu.Lock()
					accepted = append(accepted, write{base, out.SHA})
					mu.Unlock()
					base = out.SHA
				case http.StatusConflict:
					// What a client does next: take the version that beat us.
					var out struct{ SHA string }
					if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.SHA == "" {
						t.Errorf("409 carried no sha to recover from: %s", rec.Body.String())
						return
					}
					mu.Lock()
					конфликт++
					mu.Unlock()
					base = out.SHA
				default:
					t.Errorf("unexpected %d: %s", rec.Code, rec.Body.String())
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	// Contention actually happened — otherwise this proves nothing.
	if конфликт == 0 {
		t.Fatal("no writer was ever refused: the writers did not overlap, so " +
			"this measured nothing")
	}
	// Every accepted write links to the one before it.
	prev := head
	for i, w := range accepted {
		if w.base != prev {
			t.Fatalf("accepted write %d was based on %s but the head was %s — "+
				"a write that nobody saw was overwritten", i, w.base[:8], prev[:8])
		}
		prev = w.sha
	}
	t.Logf("%d accepted, %d refused, chain intact over %d writers",
		len(accepted), конфликт, writers)
}
