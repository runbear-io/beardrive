package store

import (
	"os"
	"path/filepath"
)

// The paused marker is per-device, per-volume state: `bdrive stop` sets it,
// `bdrive init` clears it. While set, nothing may sync the volume — not the
// daemon, not `bdrive sync`, and especially not the agent turn hooks, which
// would otherwise silently resume a project the user explicitly paused.
// Free functions on the volume dir so the gate is checked without opening
// (and flocking) the store.

func pausedPath(dir string) string { return filepath.Join(dir, "paused") }

// Paused reports whether syncing is paused for the volume dir.
func Paused(dir string) bool {
	_, err := os.Stat(pausedPath(dir))
	return err == nil
}

// SetPaused sets or clears the paused marker.
func SetPaused(dir string, on bool) error {
	if !on {
		err := os.Remove(pausedPath(dir))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(pausedPath(dir), []byte("paused by `bdrive stop`; `bdrive init` resumes\n"), 0o644)
}
