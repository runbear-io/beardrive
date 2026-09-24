package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// A joining device whose first download lost every blob recovers them through
// the backfill, a bounded batch per remote pass. With the change stream open a
// remote pass is 30x remoteInterval apart, so once the backfill is landing
// content the daemon has to keep passing, or a large project trickles in one
// batch per watched interval.
func TestBackfillKeepsRemotePassesComingWhileItLandsContent(t *testing.T) {
	hub := newFakeHub(t)
	const n = 600 // several backfill batches
	var jrn []byte
	for i := range n {
		content := fmt.Sprintf("file %d\n", i)
		sum := sha256.Sum256([]byte(content))
		blob := hex.EncodeToString(sum[:])
		hub.objects["blobs/"+blob] = []byte(content)
		line, err := json.Marshal(journal.Op{
			Seq: int64(i + 1), Lamport: int64(i + 1), Time: time.Now().UTC(), Device: "peer",
			Kind: journal.KindPut, Path: fmt.Sprintf("f/%03d.md", i), Blob: blob, Size: int64(len(content)), Mode: 0o644,
		})
		if err != nil {
			t.Fatal(err)
		}
		jrn = append(append(jrn, line...), '\n')
	}
	hub.objects["journal/peer.jsonl"] = jrn
	hub.failBlobs = n // the whole first download fails

	m := watchMount(t, hub)
	done := watchRun(t, m)
	defer watchStop(t, m, done)

	// Once the first cycle has spent its failures, a local edit brings the
	// next remote pass forward, and its backfill lands the first batch. The
	// watched cadence is 9s, so every later batch has to follow on its own.
	for wait := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		hub.mu.Lock()
		spent := hub.failBlobs == 0
		hub.mu.Unlock()
		if spent {
			break
		}
		if time.Now().After(wait) {
			t.Fatal("the first cycle never asked for the blobs")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(m.folder, "edit.md"), []byte("an edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for {
		ents, _ := os.ReadDir(filepath.Join(m.folder, "f"))
		if len(ents) == n {
			return
		}
		if time.Now().After(deadline) {
			hub.mu.Lock()
			events := hub.events
			hub.mu.Unlock()
			t.Fatalf("%d of %d files landed in 4s (change streams opened: %d); "+
				"the backfill is waiting out the watched poll cadence between batches", len(ents), n, events)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
