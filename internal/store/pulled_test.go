package store

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// A marker cut short (a crash mid-write, a reader racing a writer that did
// not replace the file whole) vouches for nothing.
func TestLoadPulledRejectsAnIncompleteMarker(t *testing.T) {
	for name, body := range map[string]string{
		"no final newline": "123\np-a",
		"pid only, cut":    "123",
		"no pid":           "\np-a\n",
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pulled"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if p := LoadPulled(dir); !p.At.IsZero() || p.Covers("p-a") {
				t.Errorf("an incomplete marker read as %+v", p)
			}
		})
	}
}

// The daemon renews the marker while hooks read it: every read sees a whole
// marker, the old one or the new one.
func TestPulledMarkerIsNeverReadHalfWritten(t *testing.T) {
	dir := t.TempDir()
	mounts := []string{"p-aaaaaaaa", "p-bbbbbbbb", "p-cccccccc"}
	if err := MarkPulled(dir, mounts); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = MarkPulled(dir, mounts)
			}
		}
	}()
	for range 5000 {
		p := LoadPulled(dir)
		if p.PID != os.Getpid() || !slices.Equal(p.Mounts, mounts) {
			close(stop)
			wg.Wait()
			t.Fatalf("read a partial marker: %+v", p)
		}
	}
	close(stop)
	wg.Wait()
	if left, _ := filepath.Glob(filepath.Join(dir, ".pulled-*")); len(left) != 0 {
		t.Errorf("temporary markers left behind: %v", left)
	}
}
