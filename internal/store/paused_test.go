package store

import (
	"path/filepath"
	"testing"
)

// The paused marker gates every sync path (daemon, `bdrive sync`, agent
// hooks): set by `bdrive stop`, cleared by `bdrive init`, absent by default.
func TestPaused(t *testing.T) {
	dir := t.TempDir()
	if Paused(dir) {
		t.Fatal("fresh volume dir must not be paused")
	}
	if err := SetPaused(dir, true); err != nil {
		t.Fatal(err)
	}
	if !Paused(dir) {
		t.Fatal("marker not set")
	}
	if err := SetPaused(dir, false); err != nil {
		t.Fatal(err)
	}
	if Paused(dir) {
		t.Fatal("marker not cleared")
	}
	// Clearing an already-clear volume is a no-op, not an error — init runs
	// it unconditionally.
	if err := SetPaused(dir, false); err != nil {
		t.Fatalf("double clear: %v", err)
	}
	// Stop can run before the volume dir exists (e.g. after wiping
	// ~/.bdrive/volumes by hand); the marker still sticks.
	missing := filepath.Join(dir, "never-synced")
	if err := SetPaused(missing, true); err != nil {
		t.Fatal(err)
	}
	if !Paused(missing) {
		t.Fatal("marker not set in fresh dir")
	}
}
