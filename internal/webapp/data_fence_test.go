//go:build unix

package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// A pass that began before a write may not end by reporting the index
// complete: its fold may not hold that write. The pass is held mid-fetch on a
// blob that is a FIFO while a write is counted, then let go.
func TestDataQuerySQLAPassOlderThanAWriteDoesNotReportComplete(t *testing.T) {
	h, srv, cookies, p, root := permHubAt(t)
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	srv.Data = x
	d := &dataHub{t: t, h: h, srv: srv, cookies: cookies, p: p}
	d.enable()

	body := []byte(`{"slow":true}`)
	sum := sha256.Sum256(body)
	blob := hex.EncodeToString(sum[:])
	dev := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	fifo := filepath.Join(root, p.ID, "blobs", blob)
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	dev.append("laptop", journal.Op{Kind: journal.KindPut, Path: "slow/a.json", Blob: blob, Size: int64(len(body))})

	st := srv.dataProjectState(p.ID)
	st.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- srv.dataPass(context.Background(), p.ID, st) }()
	// Opening the FIFO to write returns once the pass has opened it to read,
	// so the pass is past its start; a write is counted while it waits.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		st.mu.Unlock()
		t.Fatal(err)
	}
	st.changes.Add(1)
	w.Write(body)
	w.Close()
	err = <-done
	st.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if d.query("alice", map[string]any{"pattern": "slow/*.json", "count": true}).Complete {
		t.Fatal("a pass that began before a write reported the index complete")
	}
}
